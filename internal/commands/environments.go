package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/wolfeidau/durable/internal/githost/github"
	"github.com/wolfeidau/durable/internal/sandbox"
)

// DefaultEnvironment is the name --sandbox-image gives its image, and the one an empty environment resolves to.
const DefaultEnvironment = "default"

// EnvironmentNamePattern is the regular expression an environment name must match.
const EnvironmentNamePattern = `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`

var environmentNameRE = regexp.MustCompile(EnvironmentNamePattern)

// grantFunc builds a grant for a session; false means the grant doesn't apply to it.
type grantFunc func(access []sandbox.GitAccess) (sandbox.Grant, bool)

// grantSpec is a catalog grant: build makes it for a session (nil means a static grant from binaries/hosts), the rest is shown to operators.
type grantSpec struct {
	build    grantFunc
	summary  string
	binaries []string
	hosts    []sandbox.GrantEndpoint
}

// miseInstalls is where mise installs tools; it must match MISE_DATA_DIR in docker/worker.Dockerfile.
const miseInstalls = "/tmp/mise/data/installs"

var (
	goBinaries = []string{"/usr/local/go/bin/go", miseInstalls + "/go/*/bin/go"}
	ghBinaries = []string{"/usr/local/bin/gh"}
	// GET-only, because the mise Go path is writable, so any binary placed there gets these endpoints.
	goEndpoints = []sandbox.GrantEndpoint{
		{Host: "proxy.golang.org", Port: 443, Rules: []sandbox.HTTPRule{{Method: "GET", Path: "/**"}}},
		{Host: "sum.golang.org", Port: 443, Rules: []sandbox.HTTPRule{{Method: "GET", Path: "/**"}}},
		// The proxy redirects some module zips to signed URLs in its own bucket.
		{Host: "storage.googleapis.com", Port: 443, Rules: []sandbox.HTTPRule{{Method: "GET", Path: "/proxy-golang-org-prod/**"}}},
	}

	bunBinaries  = []string{miseInstalls + "/bun/*/bin/bun"}
	miseBinaries = []string{"/usr/local/bin/mise"}
	// Read-only lookups; the versions host replaces api.github.com for mise, and /t (telemetry) stays denied.
	miseResolveEndpoints = []sandbox.GrantEndpoint{{Host: "mise-versions.jdx.dev", Port: 443, Rules: []sandbox.HTTPRule{
		{Method: "GET", Path: "/data/*"},
		// The /** rule below doesn't match the bare list, which mise reads for tools without a data file.
		{Method: "GET", Path: "/api/github/repos/*/*/releases"},
		{Method: "GET", Path: "/api/github/repos/*/*/releases/**"},
		{Method: "GET", Path: "/api/github/repos/*/*/attestations/**"},
	}}, {
		// Sigstore's trust root, which mise fetches to verify the attestations itself.
		Host: "tuf-repo-cdn.sigstore.dev", Port: 443, Rules: []sandbox.HTTPRule{{Method: "GET", Path: "/**"}},
	}}
	npmEndpoints = []sandbox.GrantEndpoint{{Host: "registry.npmjs.org", Port: 443, Rules: []sandbox.HTTPRule{{Method: "GET", Path: "/**"}}}}
	// Every endpoint is read-only; the image runs mise locked, but that is a default the model can change, so these paths are the boundary.
	miseEndpoints = []sandbox.GrantEndpoint{
		// Release asset URLs in mise.lock, which redirect to the host below. mise sends a HEAD before each download; the gateway lets GET cover it, but older gateways may not.
		{Host: "github.com", Port: 443, Rules: []sandbox.HTTPRule{{Method: "GET", Path: "/*/*/releases/download/**"}, {Method: "HEAD", Path: "/*/*/releases/download/**"}}},
		// Where GitHub redirects release asset downloads; seen in the gateway log for aqua tools.
		{Host: "release-assets.githubusercontent.com", Port: 443, Rules: []sandbox.HTTPRule{{Method: "GET", Path: "/**"}}},
		// Node.js binaries, from mise's core node backend.
		{Host: "nodejs.org", Port: 443, Rules: []sandbox.HTTPRule{{Method: "GET", Path: "/dist/**"}}},
		// Go toolchains, from mise's core go backend.
		{Host: "dl.google.com", Port: 443, Rules: []sandbox.HTTPRule{{Method: "GET", Path: "/go/**"}}},
		// mise's own update check; without it every run retries and warns.
		{Host: "mise.jdx.dev", Port: 443, Rules: []sandbox.HTTPRule{{Method: "GET", Path: "/releases.tsv"}}},
	}
)

// grantCatalog is the grants an environment can name. The hosts are fixed here, not in the file, so a file can't widen egress.
var grantCatalog = map[string]grantSpec{
	"go-modules": {
		summary:  "go may download modules from the Go module proxy and checksum database",
		binaries: goBinaries,
		hosts:    goEndpoints,
	},
	"mise": {
		summary:  "mise, and what it runs, may download release assets from GitHub, nodejs.org/dist and dl.google.com/go, and check mise.jdx.dev for its own updates; the image installs from mise.lock by default",
		binaries: miseBinaries,
		hosts:    miseEndpoints,
	},
	"mise-resolve": {
		summary:  "mise may look up versions and attestations on mise-versions.jdx.dev, and Sigstore's trust root, so the model can add, upgrade and re-lock tools; needs mise for the downloads",
		binaries: miseBinaries,
		hosts:    miseResolveEndpoints,
	},
	"npm-registry": {
		summary:  "bun installed by mise may GET packages from the npm registry; read-only, so no publish",
		binaries: bunBinaries,
		hosts:    npmEndpoints,
	},
	"github-api": {
		build:    githubAPIGrant,
		summary:  "gh may read the session's GitHub repo and open and edit PRs and issues and comment on them, but never review or merge",
		binaries: ghBinaries,
		hosts:    []sandbox.GrantEndpoint{{Host: githubAPIHost, Port: 443}, {Host: githubAPIHost, Port: 443, Path: "/graphql"}},
	},
}

const githubAPIHost = "api.github.com"

// githubAPIMutations are the GraphQL mutations gh uses to open and edit pull requests and issues and comment. Merging
// and reviews are absent, because a review can approve a PR and so stand in for the human a merge needs.
var githubAPIMutations = []string{
	"createPullRequest", "updatePullRequest", "addComment",
	"createIssue", "updateIssue", "addLabelsToLabelable", "removeLabelsFromLabelable",
}

var githubRepoWriteRules = []sandbox.HTTPRule{
	{Method: "POST", Path: "/pulls"},
	{Method: "PATCH", Path: "/pulls/*"},
	{Method: "POST", Path: "/pulls/*/comments"},
	{Method: "POST", Path: "/issues"},
	{Method: "PATCH", Path: "/issues/*"},
	{Method: "POST", Path: "/issues/*/comments"},
	{Method: "POST", Path: "/issues/*/labels"},
	{Method: "DELETE", Path: "/issues/*/labels"},
	{Method: "POST", Path: "/issues/*/labels/*"},
	{Method: "DELETE", Path: "/issues/*/labels/*"},
}

// githubAPIGrant lets gh work on the session's GitHub repos without merging. It needs the credentialed access, because
// the token or app installation token is what gh authenticates with.
func githubAPIGrant(access []sandbox.GitAccess) (sandbox.Grant, bool) {
	rules := []sandbox.HTTPRule{{Method: "GET", Path: "/user"}, {Method: "GET", Path: "/rate_limit"}}
	baseRules := len(rules)
	var provider string
	for _, a := range access {
		if a.Kind != github.Kind || a.Provider == "" {
			continue
		}
		repo, err := github.ParseRepo(a.CloneURL)
		if err != nil {
			continue
		}
		if provider == "" {
			provider = a.Provider
		}
		base := "/repos/" + repo.Owner + "/" + repo.Name
		for _, m := range []string{"GET", "HEAD"} {
			rules = append(rules, sandbox.HTTPRule{Method: m, Path: base}, sandbox.HTTPRule{Method: m, Path: base + "/**"})
		}
		for _, r := range githubRepoWriteRules {
			rules = append(rules, sandbox.HTTPRule{Method: r.Method, Path: base + r.Path})
		}
	}
	if len(rules) == baseRules {
		return sandbox.Grant{}, false
	}
	graphql := []sandbox.GraphQLRule{{OperationType: "query"}}
	for _, f := range githubAPIMutations {
		graphql = append(graphql, sandbox.GraphQLRule{OperationType: "mutation", Fields: []string{f}})
	}
	return sandbox.Grant{
		Name:     "github-api",
		Binaries: ghBinaries,
		Provider: provider,
		Endpoints: []sandbox.GrantEndpoint{
			{Host: githubAPIHost, Port: 443, Rules: rules},
			{Host: githubAPIHost, Port: 443, Path: "/graphql", GraphQL: graphql},
		},
	}, true
}

// GrantInfo describes a catalog grant for display; Hosts are host:port or host:port/path.
type GrantInfo struct {
	Name     string
	Summary  string
	Binaries []string
	Hosts    []string
}

// GrantCatalog returns the grants an environment can name, sorted by name.
func GrantCatalog() []GrantInfo {
	var out []GrantInfo
	for _, n := range GrantNames() {
		g := grantCatalog[n]
		info := GrantInfo{Name: n, Summary: g.summary, Binaries: slices.Clone(g.binaries)}
		for _, h := range g.hosts {
			info.Hosts = append(info.Hosts, fmt.Sprintf("%s:%d%s", h.Host, h.Port, h.Path))
		}
		out = append(out, info)
	}
	return out
}

// GrantNames returns the grants an environment can name, sorted.
func GrantNames() []string { return slices.Sorted(maps.Keys(grantCatalog)) }

// Environment pairs a worker image with the network grants a session gets.
type Environment struct {
	Name   string   `json:"-"`
	Image  string   `json:"image"`
	Grants []string `json:"grants"`
	// Approvals is "ask" (also when empty) or "auto"; auto skips the approval gate in sandboxed sessions, and hooks still apply.
	Approvals string `json:"approvals,omitempty"`
}

// Values of Environment.Approvals.
const (
	ApprovalsAsk  = "ask"
	ApprovalsAuto = "auto"
)

// LoadEnvironments reads the environments file; a missing file has none.
func LoadEnvironments(path string) (map[string]Environment, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the user's own config path
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Environment{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read environments: %w", err)
	}
	var raw map[string]Environment
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("failed to parse environments: %w", err)
	}
	out := make(map[string]Environment, len(raw))
	for name, e := range raw {
		if err := validateEnvironment(name, e); err != nil {
			return nil, err
		}
		e.Name = name
		out[name] = e
	}
	return out, nil
}

func validateEnvironment(name string, e Environment) error {
	if !environmentNameRE.MatchString(name) {
		return fmt.Errorf("environment %q: name must be letters, digits, '.', '_' or '-'", name)
	}
	if strings.TrimSpace(e.Image) == "" {
		return fmt.Errorf("environment %q: image is required", name)
	}
	for _, g := range e.Grants {
		if _, ok := grantCatalog[g]; !ok {
			return fmt.Errorf("environment %q: unknown grant %q (known: %s)", name, g, strings.Join(GrantNames(), ", "))
		}
	}
	if e.Approvals != "" && e.Approvals != ApprovalsAsk && e.Approvals != ApprovalsAuto {
		return fmt.Errorf("environment %q: approvals must be %q or %q", name, ApprovalsAsk, ApprovalsAuto)
	}
	return nil
}

// ErrEnvironment is returned for an environment name that can't be resolved.
var ErrEnvironment = errors.New("unknown environment")

// ResolveEnvironment picks the named environment. An empty name means "default", or the only environment when there is exactly one.
func ResolveEnvironment(envs map[string]Environment, name string) (Environment, error) {
	if name == "" {
		if e, ok := envs[DefaultEnvironment]; ok {
			e.Name = DefaultEnvironment
			return e, nil
		}
		if len(envs) == 1 {
			for n, only := range envs {
				only.Name = n
				return only, nil
			}
		}
		return Environment{}, fmt.Errorf("%w: name one of %s", ErrEnvironment, environmentNames(envs))
	}
	e, ok := envs[name]
	if !ok {
		return Environment{}, fmt.Errorf("%w %q: available are %s", ErrEnvironment, name, environmentNames(envs))
	}
	e.Name = name
	return e, nil
}

func environmentNames(envs map[string]Environment) string {
	return strings.Join(slices.Sorted(maps.Keys(envs)), ", ")
}

// BuildGrants returns the catalog grants the environment names that apply to the session, in the environment's order.
func BuildGrants(names []string, access []sandbox.GitAccess) []sandbox.Grant {
	var out []sandbox.Grant
	for _, n := range names {
		spec, ok := grantCatalog[n]
		if !ok {
			// LoadEnvironments and config.Validate reject unknown grant names upfront.
			continue
		}
		if spec.build == nil {
			out = append(out, sandbox.Grant{Name: n, Binaries: spec.binaries, Endpoints: spec.hosts})
			continue
		}
		if g, ok := spec.build(access); ok {
			out = append(out, g)
		}
	}
	return out
}
