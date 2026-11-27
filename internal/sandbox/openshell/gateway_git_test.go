package openshell

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/commands"
	"github.com/wolfeidau/durable/internal/creds"
	"github.com/wolfeidau/durable/internal/githost/github"
	"github.com/wolfeidau/durable/internal/sandbox"
)

const (
	githubGatewayEnv = "DURABLE_TEST_OPENSHELL_GITHUB"
	githubRepoEnv    = "DURABLE_TEST_GITHUB_REPO"
	publicRepo       = "octocat/hello-world"
)

func cleanupProvider(t *testing.T, l *Launcher) {
	t.Helper()
	cleanupProviderProfile(t, l, github.Provider, github.Provider)
}

func newGitSandbox(t *testing.T, l *Launcher, repo string, grantNames ...string) (name, token string) {
	t.Helper()
	h := github.New("")
	r, err := h.ParseRepo(repo)
	require.NoError(t, err)
	access := []sandbox.GitAccess{h.Access(r)}
	cleanupProvider(t, l)
	spec := newSandboxWith(t, l, sandbox.Spec{Git: access, Grants: commands.BuildGrants(grantNames, access)})
	return spec.Name, spec.Token
}

func TestGatewayGitReachesOnlyTheGrantedRepo(t *testing.T) {
	l := gatewayLauncherWith(t, Config{Tokens: map[string]func(context.Context) (string, error){
		"github": func(context.Context) (string, error) { return "ghp_dummy_for_a_public_repo", nil },
	}})
	name, token := newGitSandbox(t, l, "github.com/"+publicRepo)
	bash := bashTool(t, dialWorker(t, l, name, token))
	run := func(id, command string) string {
		return runBash(t.Context(), t, bash, id, command+"; echo rc=$?")
	}

	out := run("g1", "git ls-remote https://github.com/"+publicRepo+".git HEAD 2>&1")
	require.Contains(t, out, "rc=0", out)

	out = run("g2", "git ls-remote https://github.com/wolfeidau/durable.git HEAD 2>&1")
	require.NotContains(t, out, "rc=0", out)
	require.Contains(t, out, "403", out)

	out = run("g3", "curl -sS -m 10 https://api.github.com 2>&1")
	require.NotContains(t, out, "rc=0", out)

	out = run("g4", "env | grep '^GITHUB_TOKEN='")
	require.Contains(t, out, "GITHUB_TOKEN=openshell:resolve", out)
	require.NotContains(t, out, "ghp_dummy", out)
}

// mise and git both reach github.com, which the gateway accepts only when the two endpoints share a binding.
func TestGatewayGitWithMiseGrantInstallsFromGitHub(t *testing.T) {
	l := gatewayLauncherWith(t, Config{Tokens: map[string]func(context.Context) (string, error){
		"github": func(context.Context) (string, error) { return "ghp_dummy_for_a_public_repo", nil },
	}})
	name, token := newGitSandbox(t, l, "github.com/"+publicRepo, "mise")
	bash := bashTool(t, dialWorker(t, l, name, token))

	run := func(id, command string) string {
		return runBash(t.Context(), t, bash, id, command+" 2>&1; echo rc=$?")
	}

	out := run("gm1", "git ls-remote https://github.com/"+publicRepo+".git HEAD")
	require.Contains(t, out, "rc=0", out)

	out = run("gm2", "echo "+b64(miseFixtureConfig)+" | base64 -d > mise.toml && echo "+b64(miseFixtureLock)+" | base64 -d > mise.lock")
	require.Contains(t, out, "rc=0", out)

	// The release download now goes through the bound github.com endpoint.
	out = run("gm3", "mise install && mise exec -- jq --version")
	require.Contains(t, out, "rc=0", out)
	require.NotContains(t, out, "policy_denied", out)
}

func TestGatewayGitPushesToTheGrantedRepo(t *testing.T) {
	repo := os.Getenv(githubRepoEnv)
	if os.Getenv(githubGatewayEnv) != "1" || repo == "" {
		t.Skipf("set %s=1 and %s=<owner>/<repo> to push with a realToken token", githubGatewayEnv, githubRepoEnv)
	}
	// The token is read from gh and never printed.
	ghToken := func(ctx context.Context) (string, error) {
		return creds.Resolve(ctx, creds.Source{Flag: "gh", Env: "GH_UNUSED", Value: "gh"})
	}
	realToken, err := ghToken(t.Context())
	require.NoError(t, err)
	l := gatewayLauncherWith(t, Config{Tokens: map[string]func(context.Context) (string, error){"github": ghToken}})
	name, token := newGitSandbox(t, l, "github.com/"+repo)
	bash := bashTool(t, dialWorker(t, l, name, token))
	branch := "durable-test-" + randHex(t, 4)

	script := strings.Join([]string{
		"cd /sandbox",
		"git clone -q https://github.com/" + repo + ".git work 2>&1",
		"cd work",
		"git -c user.name=durable -c user.email=durable@example.invalid commit --allow-empty -q -m 'durable gateway test' 2>&1",
		"git push -q origin HEAD:refs/heads/" + branch + " 2>&1",
		"git push -q origin --delete " + branch + " 2>&1",
	}, " && ")
	out := runBash(t.Context(), t, bash, "p1", script+"; echo rc=$?")
	require.Contains(t, out, "rc=0", "push failed")
	require.NotContains(t, out, realToken)

	out = runBash(t.Context(), t, bash, "p2", "env")
	require.Contains(t, out, "GITHUB_TOKEN=openshell:resolve")
	require.NotContains(t, out, realToken)
}

func TestGatewayGitHubAPIGrantAllowsTheRepoAndDeniesMerge(t *testing.T) {
	repo := strings.ToLower(os.Getenv(githubRepoEnv))
	if os.Getenv(githubGatewayEnv) != "1" || repo == "" {
		t.Skipf("set %s=1 and %s=<owner>/<repo> to use the github-api grant with a real token", githubGatewayEnv, githubRepoEnv)
	}
	// The token is read from gh and never printed.
	ghToken := func(ctx context.Context) (string, error) {
		return creds.Resolve(ctx, creds.Source{Flag: "gh", Env: "GH_UNUSED", Value: "gh"})
	}
	realToken, err := ghToken(t.Context())
	require.NoError(t, err)
	l := gatewayLauncherWith(t, Config{Tokens: map[string]func(context.Context) (string, error){"github": ghToken}})
	name, token := newGitSandbox(t, l, "github.com/"+repo, "github-api")
	bash := bashTool(t, dialWorker(t, l, name, token))
	run := newBashRunner(t, bash, "a", realToken)

	out := run("gh api repos/" + repo + " --jq .full_name")
	require.Contains(t, out, "rc=0", out)

	out = run("gh pr list --repo " + repo)
	require.Contains(t, out, "rc=0", out)
	require.NotContains(t, out, "policy_denied", out)

	out = run("gh api -X PUT repos/" + repo + "/pulls/1/merge")
	require.NotContains(t, out, "rc=0", out)
	require.Contains(t, out, "policy_denied", "the policy, not GitHub, must refuse the merge: "+out)

	out = run(`gh api graphql -f query='mutation { mergePullRequest(input: {pullRequestId: "x"}) { clientMutationId } }'`)
	require.NotContains(t, out, "rc=0", out)
	require.Contains(t, out, "policy_denied", "the policy, not GitHub, must refuse the mutation: "+out)

	// An allowed mutation with a bogus id reaches GitHub, which rejects the id; the policy lets it through.
	out = run(`gh api graphql -f query='mutation { addComment(input: {subjectId: "x", body: "y"}) { clientMutationId } }'`)
	require.NotContains(t, out, "policy_denied", out)
	require.Contains(t, out, "NOT_FOUND", "GitHub, not the policy, must answer the allowed mutation: "+out)

	out = run(`gh api graphql -f query='mutation { addPullRequestReview(input: {pullRequestId: "x", event: APPROVE}) { clientMutationId } }'`)
	require.NotContains(t, out, "rc=0", out)
	require.Contains(t, out, "policy_denied", "a review can approve a PR, so the policy must refuse it: "+out)

	out = run("gh api repos/cli/cli")
	require.NotContains(t, out, "rc=0", out)
	require.Contains(t, out, "policy_denied", out)

	out = run("curl -sS -m 10 https://api.github.com/zen")
	require.NotContains(t, out, "rc=0", out)
}
