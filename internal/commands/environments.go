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
type grantFunc func(workspace string, access []sandbox.GitAccess) (sandbox.Grant, bool)

// grantSpec is a catalog grant: build makes it for a session, the rest is shown to operators.
type grantSpec struct {
	build    grantFunc
	summary  string
	binaries []string
	hosts    []sandbox.GrantEndpoint
}

var (
	goBinaries  = []string{"/usr/local/go/bin/go"}
	ghBinaries  = []string{"/usr/local/bin/gh"}
	goEndpoints = []sandbox.GrantEndpoint{{Host: "proxy.golang.org", Port: 443}, {Host: "sum.golang.org", Port: 443}}
)

// grantCatalog is the grants an environment can name. The hosts are fixed here, not in the file, so a file can't widen egress.
var grantCatalog = map[string]grantSpec{
	"go-modules": {
		build: func(string, []sandbox.GitAccess) (sandbox.Grant, bool) {
			return sandbox.Grant{Name: "go-modules", Binaries: goBinaries, Endpoints: goEndpoints}, true
		},
		summary:  "go may download modules from the Go module proxy and checksum database",
		binaries: goBinaries,
		hosts:    goEndpoints,
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

// githubAPIGrant lets gh work on the session's GitHub repos without merging. It needs the credentialed access, because
// the token is what gh authenticates with.
func githubAPIGrant(_ string, access []sandbox.GitAccess) (sandbox.Grant, bool) {
	rules := []sandbox.HTTPRule{{Method: "GET", Path: "/user"}, {Method: "GET", Path: "/rate_limit"}}
	baseRules := len(rules)
	for _, a := range access {
		if a.Kind != github.Kind || a.Provider != github.Provider {
			continue
		}
		repo, err := github.New("").ParseRepo(a.CloneURL)
		if err != nil {
			continue
		}
		base := "/repos/" + repo.Owner + "/" + repo.Name
		for _, m := range []string{"GET", "HEAD"} {
			rules = append(rules, sandbox.HTTPRule{Method: m, Path: base}, sandbox.HTTPRule{Method: m, Path: base + "/**"})
		}
		rules = append(rules,
			sandbox.HTTPRule{Method: "POST", Path: base + "/pulls"},
			sandbox.HTTPRule{Method: "PATCH", Path: base + "/pulls/*"},
			sandbox.HTTPRule{Method: "POST", Path: base + "/pulls/*/comments"},
			sandbox.HTTPRule{Method: "POST", Path: base + "/issues"},
			sandbox.HTTPRule{Method: "PATCH", Path: base + "/issues/*"},
			sandbox.HTTPRule{Method: "POST", Path: base + "/issues/*/comments"},
			sandbox.HTTPRule{Method: "POST", Path: base + "/issues/*/labels"},
			sandbox.HTTPRule{Method: "DELETE", Path: base + "/issues/*/labels"},
			sandbox.HTTPRule{Method: "POST", Path: base + "/issues/*/labels/*"},
			sandbox.HTTPRule{Method: "DELETE", Path: base + "/issues/*/labels/*"},
		)
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
		Provider: github.Provider,
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
}

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
		if !environmentNameRE.MatchString(name) {
			return nil, fmt.Errorf("environment %q: name must be letters, digits, '.', '_' or '-'", name)
		}
		if strings.TrimSpace(e.Image) == "" {
			return nil, fmt.Errorf("environment %q: image is required", name)
		}
		for _, g := range e.Grants {
			if _, ok := grantCatalog[g]; !ok {
				return nil, fmt.Errorf("environment %q: unknown grant %q (known: %s)", name, g, strings.Join(GrantNames(), ", "))
			}
		}
		e.Name = name
		out[name] = e
	}
	return out, nil
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
func BuildGrants(names []string, workspace string, access []sandbox.GitAccess) []sandbox.Grant {
	var out []sandbox.Grant
	for _, n := range names {
		if g, ok := grantCatalog[n].build(workspace, access); ok {
			out = append(out, g)
		}
	}
	return out
}
