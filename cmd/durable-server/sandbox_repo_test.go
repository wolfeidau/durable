package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/creds"
)

func TestPlainServerRefusesRepoSessions(t *testing.T) {
	_, home := newWorkspace(t)
	c := newClient(startIn(t, home))

	_, err := c.createRepo(t, "github.com/octocat/hello-world", "master")

	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err), "%v", err)
}

func TestSandboxedServerRejectsAnInvalidBranch(t *testing.T) {
	_, home := newWorkspace(t)
	api := newFakeAPI(t, textReply("DONE"), 0)
	env := append(apiEnv(api), "XDG_CONFIG_HOME="+t.TempDir(), "DURABLE_GITHUB_TOKEN=")
	c := newClient(startEnv(t, home, env, "--api-token", "dummy", "--sandbox-image", "durable-worker:dev"))

	for _, branch := range []string{"", "-x", "a..b"} {
		_, err := c.createRepo(t, "github.com/octocat/hello-world", branch)
		require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "%q: %v", branch, err)
	}
	_, err := c.createRepo(t, "https://example.com/o/n", "main")
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "%v", err)
}

func TestSandboxImageWithoutGatewayFailsRepoCreateGenerically(t *testing.T) {
	_, home := newWorkspace(t)
	api := newFakeAPI(t, textReply("DONE"), 0)
	env := append(apiEnv(api), "XDG_CONFIG_HOME="+t.TempDir(), "DURABLE_GITHUB_TOKEN=")
	r := startEnv(t, home, env, "--api-token", "dummy", "--sandbox-image", "durable-worker:dev")

	_, err := newClient(r).createRepo(t, "github.com/octocat/hello-world", "master")

	require.Equal(t, connect.CodeUnavailable, connect.CodeOf(err), "%v", err)
	require.EqualError(t, err, "unavailable: sandbox unavailable: see the server log")
	pins, gerr := filepath.Glob(filepath.Join(home, ".durable", "sessions", "*", "sandbox.json"))
	require.NoError(t, gerr)
	require.Len(t, pins, 1, "the pin is written before the sandbox is created")
}

func TestGatewayRepoSessionClonesPublicRepo(t *testing.T) {
	_, home, l := gatewayHome(t)
	api := sequenceAPI(t,
		toolReply("toolu_bash1", "bash", bashInput(t, "ls; git -C /sandbox rev-parse --abbrev-ref HEAD")),
		textReply("done"),
	)
	// An empty token variable keeps the developer's own token out, so the session is anonymous.
	r := startEnv(t, home, append(apiEnv(api), "DURABLE_GITHUB_TOKEN="), "--api-token", "dummy", "--sandbox-image", gatewayImage())
	c := newClient(r)

	got, err := c.createRepo(t, "github.com/octocat/hello-world", "master")
	require.NoError(t, err)
	id := got.GetSession().GetId()
	sessionPin(t, home, id, l)

	stream := openWatch(t, r, id, false)
	submitAs(t, c, id, "look")
	approveSeq(t, c, id, 1)
	items := lastRun(t, collectRuns(t, stream, 1))

	out, ok := toolResult(items, "toolu_bash1")
	require.True(t, ok)
	require.Contains(t, out, "README")
	require.Contains(t, out, "master")
}

func TestGatewayRepoSessionPushes(t *testing.T) {
	_, home, l := gatewayHome(t)
	repo := os.Getenv("DURABLE_TEST_GITHUB_REPO")
	if os.Getenv("DURABLE_TEST_OPENSHELL_GITHUB") != "1" || repo == "" {
		t.Skip("set DURABLE_TEST_OPENSHELL_GITHUB=1 and DURABLE_TEST_GITHUB_REPO=<owner>/<repo> to push to GitHub")
	}
	// The temporary HOME hides gh's keychain on macOS, so the token is read here and passed in the environment.
	token, err := creds.Resolve(t.Context(), creds.Source{Flag: "test", Value: "gh"})
	require.NoError(t, err)
	suffix := make([]byte, 4)
	_, err = rand.Read(suffix)
	require.NoError(t, err)
	branch := "durable-test-" + hex.EncodeToString(suffix)
	command := "set -e; cd /sandbox; git -c user.name=durable -c user.email=durable@example.com commit --allow-empty -qm test; " +
		"git push -q origin HEAD:refs/heads/" + branch + "; git push -q origin --delete " + branch + "; echo rc=0"
	api := sequenceAPI(t,
		toolReply("toolu_bash1", "bash", bashInput(t, command)),
		textReply("done"),
	)
	r := startEnv(t, home, append(apiEnv(api), "DURABLE_GITHUB_TOKEN="+token), "--api-token", "dummy", "--sandbox-image", gatewayImage())
	c := newClient(r)

	got, err := c.createRepo(t, "github.com/"+repo, "main")
	require.NoError(t, err)
	id := got.GetSession().GetId()
	sessionPin(t, home, id, l)

	stream := openWatch(t, r, id, false)
	submitAs(t, c, id, "push")
	approveSeq(t, c, id, 1)
	items := lastRun(t, collectRuns(t, stream, 1))

	out, ok := toolResult(items, "toolu_bash1")
	require.True(t, ok)
	require.Contains(t, out, "rc=0")
	require.NotContains(t, out, token)
}
