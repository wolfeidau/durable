package openshell

import (
	"encoding/json"
	"testing"

	"github.com/ai-openshell/sdk-go/openshell/v1/fake"
	"github.com/ai-openshell/sdk-go/openshell/v1/types"
	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/sandbox"
)

func TestCreateEmitsOneRulePerGrant(t *testing.T) {
	fc := fake.NewClient()
	grants := []sandbox.Grant{
		{Name: "go-modules", Binaries: []string{"/usr/local/go/bin/go"}, Endpoints: []sandbox.GrantEndpoint{{Host: "proxy.golang.org", Port: 443}, {Host: "sum.golang.org", Port: 443}}},
		{Name: "other", Binaries: []string{"/usr/bin/curl"}, Endpoints: []sandbox.GrantEndpoint{{Host: "example.com", Port: 443}}},
	}

	require.NoError(t, newWithClient(fc, "", nil).Create(t.Context(), sandbox.Spec{Name: "dt-a", Image: "img", Token: "t", Grants: grants}))

	sb, err := fc.Sandboxes().Get(t.Context(), "default", "dt-a")
	require.NoError(t, err)
	require.Equal(t, map[string]types.NetworkPolicyRule{
		"durable-grant-go-modules": {
			Name: "durable-grant-go-modules",
			Endpoints: []types.PolicyNetworkEndpoint{
				{Host: "proxy.golang.org", Port: 443},
				{Host: "sum.golang.org", Port: 443},
			},
			Binaries: []types.PolicyNetworkBinary{{Path: "/usr/local/go/bin/go"}},
		},
		"durable-grant-other": {
			Name:      "durable-grant-other",
			Endpoints: []types.PolicyNetworkEndpoint{{Host: "example.com", Port: 443}},
			Binaries:  []types.PolicyNetworkBinary{{Path: "/usr/bin/curl"}},
		},
	}, sb.Spec.Policy.NetworkPolicies)
}

func TestGrantWithRulesIsEnforcedL7WithBinding(t *testing.T) {
	rec := newRecorder()
	token := "ghp_one"
	l := newWithClient(rec, "", tokenFn(&token))
	access := repoAccess(t, "github.com/octo/hello")
	grant := sandbox.Grant{
		Name: "api", Binaries: []string{"/usr/bin/curl"}, Provider: access.Provider,
		Endpoints: []sandbox.GrantEndpoint{{Host: "api.example.com", Port: 443, Rules: []sandbox.HTTPRule{{Method: "GET", Path: "/**"}}}},
	}

	require.NoError(t, l.Create(t.Context(), sandbox.Spec{Name: "dt-a", Image: "img", Token: "wt", Git: []sandbox.GitAccess{access}, Grants: []sandbox.Grant{grant}}))

	sb, err := rec.Sandboxes().Get(t.Context(), "default", "dt-a")
	require.NoError(t, err)
	rule := sb.Spec.Policy.NetworkPolicies["durable-grant-api"]
	require.Equal(t, []types.PolicyNetworkEndpoint{{
		Host: "api.example.com", Port: 443, Protocol: "rest", Enforcement: types.NetworkEnforcementModeEnforce,
		Rules:             []types.L7Rule{{Allow: &types.L7Allow{Method: "GET", Path: "/**"}}},
		CredentialBinding: &types.NetworkCredentialBinding{Provider: access.Provider},
	}}, rule.Endpoints)
	require.Contains(t, sb.Spec.Policy.NetworkPolicies, "durable-git")
	require.Equal(t, []string{access.Provider}, sb.Spec.Providers)
}

func TestGrantProviderWithoutGitAccessFailsCreate(t *testing.T) {
	fc := fake.NewClient()
	grant := sandbox.Grant{Name: "api", Binaries: []string{"/usr/bin/curl"}, Provider: "durable-github", Endpoints: []sandbox.GrantEndpoint{{Host: "api.example.com", Port: 443}}}

	err := newWithClient(fc, "", nil).Create(t.Context(), sandbox.Spec{Name: "dt-a", Image: "img", Token: "t", Grants: []sandbox.Grant{grant}})

	require.ErrorContains(t, err, `grant "api" needs provider "durable-github"`)
	_, err = fc.Sandboxes().Get(t.Context(), "default", "dt-a")
	require.Error(t, err, "no sandbox is created")
}

func TestGrantRoutesGraphQLAndRESTOnOneHost(t *testing.T) {
	rec := newRecorder()
	token := "ghp_one"
	l := newWithClient(rec, "", tokenFn(&token))
	access := repoAccess(t, "github.com/octo/hello")
	grant := sandbox.Grant{
		Name: "api", Binaries: []string{"/usr/local/bin/gh"}, Provider: access.Provider,
		Endpoints: []sandbox.GrantEndpoint{
			{Host: "api.example.com", Port: 443, Rules: []sandbox.HTTPRule{{Method: "GET", Path: "/repos/**"}}},
			{Host: "api.example.com", Port: 443, Path: "/graphql", GraphQL: []sandbox.GraphQLRule{
				{OperationType: "query"},
				{OperationType: "mutation", Fields: []string{"createIssue"}},
			}},
		},
	}

	require.NoError(t, l.Create(t.Context(), sandbox.Spec{Name: "dt-a", Image: "img", Token: "wt", Git: []sandbox.GitAccess{access}, Grants: []sandbox.Grant{grant}}))

	sb, err := rec.Sandboxes().Get(t.Context(), "default", "dt-a")
	require.NoError(t, err)
	binding := &types.NetworkCredentialBinding{Provider: access.Provider}
	require.Equal(t, []types.PolicyNetworkEndpoint{
		{
			Host: "api.example.com", Port: 443, Protocol: "rest", Enforcement: types.NetworkEnforcementModeEnforce,
			Rules:             []types.L7Rule{{Allow: &types.L7Allow{Method: "GET", Path: "/repos/**"}}},
			CredentialBinding: binding,
		},
		{
			Host: "api.example.com", Port: 443, Path: "/graphql", Protocol: "graphql", Enforcement: types.NetworkEnforcementModeEnforce,
			Rules: []types.L7Rule{
				{Allow: &types.L7Allow{OperationType: "query"}},
				{Allow: &types.L7Allow{OperationType: "mutation", Fields: []string{"createIssue"}}},
			},
			CredentialBinding: binding,
		},
	}, sb.Spec.Policy.NetworkPolicies["durable-grant-api"].Endpoints)
}

func TestGrantFromAnOldPinStillDecodesAndRenders(t *testing.T) {
	var g sandbox.Grant
	require.NoError(t, json.Unmarshal([]byte(`{"name":"go-modules","binaries":["/usr/local/go/bin/go"],"endpoints":[{"host":"proxy.golang.org","port":443}]}`), &g))

	require.Equal(t, sandbox.Grant{
		Name: "go-modules", Binaries: []string{"/usr/local/go/bin/go"},
		Endpoints: []sandbox.GrantEndpoint{{Host: "proxy.golang.org", Port: 443}},
	}, g)
	require.Equal(t, types.NetworkPolicyRule{
		Name:      "durable-grant-go-modules",
		Endpoints: []types.PolicyNetworkEndpoint{{Host: "proxy.golang.org", Port: 443}},
		Binaries:  []types.PolicyNetworkBinary{{Path: "/usr/local/go/bin/go"}},
	}, grantPolicyRule(g))
}
