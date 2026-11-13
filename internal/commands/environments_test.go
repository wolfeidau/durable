package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/githost"
	"github.com/wolfeidau/durable/internal/githost/github"
	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/sandbox/sandboxtest"
)

// imageEnvs is one "default" environment, or none for an empty image.
func imageEnvs(image string) map[string]Environment {
	if image == "" {
		return nil
	}
	return map[string]Environment{DefaultEnvironment: {Name: DefaultEnvironment, Image: image}}
}

func writeEnvFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "environments.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestLoadEnvironments(t *testing.T) {
	envs, err := LoadEnvironments(writeEnvFile(t, `{"go": {"image": "durable-worker-go:dev", "grants": ["go-modules"]}, "plain": {"image": "img"}}`))

	require.NoError(t, err)
	require.Equal(t, map[string]Environment{
		"go":    {Name: "go", Image: "durable-worker-go:dev", Grants: []string{"go-modules"}},
		"plain": {Name: "plain", Image: "img"},
	}, envs)
}

func TestLoadEnvironmentsMissingFileIsEmpty(t *testing.T) {
	envs, err := LoadEnvironments(filepath.Join(t.TempDir(), "none.json"))

	require.NoError(t, err)
	require.Empty(t, envs)
}

func TestLoadEnvironmentsRejects(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"unknown grant": {`{"a": {"image": "img", "grants": ["nope"]}}`, `unknown grant "nope"`},
		"empty image":   {`{"a": {"image": " "}}`, "image is required"},
		"missing image": {`{"a": {}}`, "image is required"},
		"invalid name":  {`{"a b": {"image": "img"}}`, "name must be"},
		"leading dash":  {`{"-a": {"image": "img"}}`, "name must be"},
		"unknown field": {`{"a": {"image": "img", "egress": ["x"]}}`, "unknown field"},
		"not json":      {`nope`, "failed to parse environments"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadEnvironments(writeEnvFile(t, tc.body))

			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestResolveEnvironment(t *testing.T) {
	def := map[string]Environment{"default": {Image: "d"}, "go": {Image: "g"}}
	two := map[string]Environment{"go": {Image: "g"}, "rust": {Image: "r"}}
	one := map[string]Environment{"go": {Image: "g"}}

	for name, tc := range map[string]struct {
		envs      map[string]Environment
		ask, want string
		wantErr   string
	}{
		"empty picks default":    {def, "", "default", ""},
		"empty picks the only":   {one, "", "go", ""},
		"named":                  {def, "go", "go", ""},
		"empty is ambiguous":     {two, "", "", "name one of go, rust"},
		"empty with none":        {nil, "", "", "unknown environment"},
		"unknown names the rest": {two, "c", "", `unknown environment "c": available are go, rust`},
	} {
		t.Run(name, func(t *testing.T) {
			e, err := ResolveEnvironment(tc.envs, tc.ask)

			if tc.wantErr != "" {
				require.ErrorIs(t, err, ErrEnvironment)
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, e.Name)
		})
	}
}

func goEnvs() map[string]Environment {
	return map[string]Environment{"go": {Image: "go-img", Grants: []string{"go-modules"}}}
}

func TestAssemblePinsEnvironmentImageAndGrants(t *testing.T) {
	assembleHome(t)
	f := sandboxtest.New(t)
	cfg := sandboxCfg(t, f, "")
	cfg.Sandbox.Environments = goEnvs()

	a, err := Assemble(t.Context(), cfg)
	require.NoError(t, err)
	a.Close()

	spec := f.Specs()[0]
	require.Equal(t, "go-img", spec.Image)
	require.Equal(t, []sandbox.Grant{{
		Name:      "go-modules",
		Binaries:  []string{"/usr/local/go/bin/go"},
		Endpoints: []sandbox.GrantEndpoint{{Host: "proxy.golang.org", Port: 443}, {Host: "sum.golang.org", Port: 443}},
	}}, spec.Grants)
	pin, ok, err := sandbox.ReadPin(filepath.Dir(mustSessionPath(t, cfg)))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, spec.Grants, pin.Grants)
}

func githubAccess(t *testing.T, repo string) sandbox.GitAccess {
	t.Helper()
	h := github.New("")
	r, err := h.ParseRepo("github.com/" + repo)
	require.NoError(t, err)
	return h.Access(r)
}

func TestGitHubAPIGrantNeedsCredentialedGitHubAccess(t *testing.T) {
	_, ok := grantCatalog["github-api"].build("", nil)
	require.False(t, ok)

	_, ok = grantCatalog["github-api"].build("", []sandbox.GitAccess{githost.Anonymous(githubAccess(t, "octo/hello"))})
	require.False(t, ok, "an anonymous clone has no token for gh")

	_, ok = grantCatalog["github-api"].build("", []sandbox.GitAccess{{Kind: "gitlab", Provider: github.Provider, CloneURL: "https://github.com/octo/hello.git"}})
	require.False(t, ok)

	g, ok := grantCatalog["github-api"].build("", []sandbox.GitAccess{githubAccess(t, "octo/hello")})
	require.True(t, ok)
	require.Equal(t, "github-api", g.Name)
	require.Equal(t, []string{"/usr/local/bin/gh"}, g.Binaries)
	require.Equal(t, github.Provider, g.Provider)
}

func TestGitHubAPIGrantRESTRulesAreScopedToTheRepo(t *testing.T) {
	g, _ := grantCatalog["github-api"].build("", []sandbox.GitAccess{githubAccess(t, "octo/hello")})

	require.Len(t, g.Endpoints, 2)
	rest := g.Endpoints[0]
	require.Equal(t, sandbox.GrantEndpoint{Host: "api.github.com", Port: 443, Rules: rest.Rules}, rest)
	for _, r := range []sandbox.HTTPRule{
		{Method: "GET", Path: "/user"},
		{Method: "GET", Path: "/rate_limit"},
		{Method: "GET", Path: "/repos/octo/hello"},
		{Method: "HEAD", Path: "/repos/octo/hello/**"},
		{Method: "POST", Path: "/repos/octo/hello/pulls"},
		{Method: "PATCH", Path: "/repos/octo/hello/pulls/*"},
		{Method: "POST", Path: "/repos/octo/hello/issues/*/comments"},
		{Method: "DELETE", Path: "/repos/octo/hello/issues/*/labels/*"},
	} {
		require.Contains(t, rest.Rules, r)
	}
	for _, r := range rest.Rules {
		require.NotEqual(t, "PUT", r.Method)
		if r.Path != "/user" && r.Path != "/rate_limit" {
			require.Truef(t, strings.HasPrefix(r.Path, "/repos/octo/hello"), "rule outside the repo: %v", r)
		}
		require.NotContains(t, r.Path, "merge")
		require.NotContains(t, r.Path, "releases")
		require.NotContains(t, r.Path, "contents")
		require.NotContains(t, r.Path, "reviews", "a review can approve a PR")
	}
}

func TestGitHubAPIGrantHasOneRuleSetPerRepo(t *testing.T) {
	g, _ := grantCatalog["github-api"].build("", []sandbox.GitAccess{githubAccess(t, "octo/hello"), githubAccess(t, "octo/world")})

	paths := ""
	for _, r := range g.Endpoints[0].Rules {
		paths += r.Path + "\n"
	}
	require.Contains(t, paths, "/repos/octo/hello/pulls\n")
	require.Contains(t, paths, "/repos/octo/world/pulls\n")
}

func TestGitHubAPIGrantGraphQLQueriesAndNamedMutationsOnly(t *testing.T) {
	g, _ := grantCatalog["github-api"].build("", []sandbox.GitAccess{githubAccess(t, "octo/hello")})

	gql := g.Endpoints[1]
	require.Equal(t, "api.github.com", gql.Host)
	require.Equal(t, "/graphql", gql.Path)
	require.Empty(t, gql.Rules)
	require.Contains(t, gql.GraphQL, sandbox.GraphQLRule{OperationType: "query"})
	var fields []string
	for _, r := range gql.GraphQL {
		if r.OperationType == "mutation" {
			require.Len(t, r.Fields, 1, "a mutation rule without fields would allow every mutation")
			fields = append(fields, r.Fields[0])
		} else {
			require.Equal(t, "query", r.OperationType)
		}
	}
	require.ElementsMatch(t, []string{
		"createPullRequest", "updatePullRequest", "addComment",
		"createIssue", "updateIssue", "addLabelsToLabelable", "removeLabelsFromLabelable",
	}, fields)
	require.NotContains(t, fields, "mergePullRequest")
	require.NotContains(t, fields, "enablePullRequestAutoMerge")
	require.NotContains(t, fields, "addPullRequestReview", "a review can approve a PR")
}

func TestBuildGrantsSkipsGitHubAPIWithoutGitHubAccess(t *testing.T) {
	require.Empty(t, BuildGrants([]string{"github-api"}, "", nil))
	got := BuildGrants([]string{"go-modules", "github-api"}, "", []sandbox.GitAccess{githubAccess(t, "octo/hello")})
	require.Len(t, got, 2)
	require.Equal(t, "github-api", got[1].Name)
}

func mustSessionPath(t *testing.T, cfg AssembleConfig) string {
	t.Helper()
	p, err := SessionPath(cfg.CWD, cfg.Session)
	require.NoError(t, err)
	return p
}

func TestAssembleReopenKeepsPinWhenEnvironmentsChange(t *testing.T) {
	for name, after := range map[string]map[string]Environment{
		"changed": {"go": {Image: "other-img"}},
		"removed": nil,
	} {
		t.Run(name, func(t *testing.T) {
			assembleHome(t)
			f := sandboxtest.New(t)
			cfg := sandboxCfg(t, f, "")
			cfg.Sandbox.Environments = goEnvs()
			cfg.Environment = "go"
			a, err := Assemble(t.Context(), cfg)
			require.NoError(t, err)
			a.Close()
			markExisting(t, cfg)

			cfg.Sandbox.Environments = after
			a, err = Assemble(t.Context(), cfg)
			require.NoError(t, err)
			defer a.Close()

			specs := f.Specs()
			require.Equal(t, specs[0], specs[1])
			require.Equal(t, "go-img", specs[1].Image)
			require.Len(t, specs[1].Grants, 1)
		})
	}
}

func TestAssembleNewSessionResolvesEnvironment(t *testing.T) {
	cfg := func(f *sandboxtest.Fake, envs map[string]Environment, name string) AssembleConfig {
		c := sandboxCfg(t, f, "")
		c.Sandbox.Environments = envs
		c.Environment = name
		return c
	}
	two := map[string]Environment{"go": {Image: "g"}, "rust": {Image: "r"}}

	t.Run("unknown is refused before any pin", func(t *testing.T) {
		assembleHome(t)
		f := sandboxtest.New(t)
		c := cfg(f, two, "nope")

		_, err := Assemble(t.Context(), c)

		require.ErrorIs(t, err, ErrEnvironment)
		_, ok, perr := sandbox.ReadPin(filepath.Dir(mustSessionPath(t, c)))
		require.NoError(t, perr)
		require.False(t, ok)
	})

	t.Run("ambiguous empty is refused", func(t *testing.T) {
		assembleHome(t)
		_, err := Assemble(t.Context(), cfg(sandboxtest.New(t), two, ""))

		require.ErrorIs(t, err, ErrEnvironment)
	})

	t.Run("named is used", func(t *testing.T) {
		assembleHome(t)
		f := sandboxtest.New(t)

		a, err := Assemble(t.Context(), cfg(f, two, "rust"))
		require.NoError(t, err)
		a.Close()

		require.Equal(t, "r", f.Specs()[0].Image)
	})
}
