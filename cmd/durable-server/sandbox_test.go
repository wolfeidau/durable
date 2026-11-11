package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
)

const unknownSessionID = "0123456789abcdef"

// sandboxDelete runs durable-server sandbox-delete and returns its combined output.
func sandboxDelete(t *testing.T, home, id string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), binPath, "sandbox-delete", id)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestSandboxDeleteRejectsBadSessions(t *testing.T) {
	_, home := newWorkspace(t)
	withoutPin := filepath.Join(home, ".durable", "sessions", "aaaaaaaaaaaaaaaa")
	require.NoError(t, os.MkdirAll(withoutPin, 0o700))

	tests := []struct{ name, id, want string }{
		{name: "invalid id", id: "../x", want: "invalid session id"},
		{name: "unknown session", id: unknownSessionID, want: "has no sandbox"},
		{name: "no sandbox.json", id: "aaaaaaaaaaaaaaaa", want: "has no sandbox"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := sandboxDelete(t, home, tc.id)
			require.Error(t, err, out)
			require.Contains(t, out, tc.want)
		})
	}
}

func TestSandboxDeleteRefusesWhileServerRuns(t *testing.T) {
	_, home := newWorkspace(t)
	startIn(t, home)

	out, err := sandboxDelete(t, home, unknownSessionID)

	require.Error(t, err, out)
	require.Contains(t, out, "another durable-server is running")
}

func TestSandboxImageWithoutGatewayFailsCreateGenerically(t *testing.T) {
	ws, home := newWorkspace(t)
	api := newFakeAPI(t, textReply("DONE"), 0)
	// No gateway config, so the lazy connect fails whatever the developer's machine has.
	env := append(apiEnv(api), "XDG_CONFIG_HOME="+t.TempDir())
	r := startEnv(t, home, env, "--api-token", "dummy", "--sandbox-image", "durable-worker:dev")

	c := newClient(r)
	require.Equal(t, []v1.Workspace{v1.Workspace_WORKSPACE_EMPTY, v1.Workspace_WORKSPACE_REPO}, c.workspaces(t))

	_, err := c.createEmpty(t, "", "req-0123456789abcdef")

	require.Equal(t, connect.CodeUnavailable, connect.CodeOf(err), "%v", err)
	require.EqualError(t, err, "unavailable: sandbox unavailable: see the server log")
	pinGlob := filepath.Join(home, ".durable", "sessions", "*", "sandbox.json")
	pins, gerr := filepath.Glob(pinGlob)
	require.NoError(t, gerr)
	require.Len(t, pins, 1, "the pin is written before the sandbox is created")
	first, rerr := os.ReadFile(pins[0])
	require.NoError(t, rerr)

	_, err = c.createEmpty(t, "other name", "req-0123456789abcdef")
	require.Equal(t, connect.CodeUnavailable, connect.CodeOf(err), "%v", err)
	pins, gerr = filepath.Glob(pinGlob)
	require.NoError(t, gerr)
	require.Len(t, pins, 1, "the retry reuses the session directory")
	again, rerr := os.ReadFile(pins[0])
	require.NoError(t, rerr)
	require.Equal(t, string(first), string(again), "the retry reuses the sandbox name and token")
	dirs, gerr := filepath.Glob(filepath.Join(home, ".durable", "sessions", "*"))
	require.NoError(t, gerr)
	require.Len(t, dirs, 1)

	_, err = c.create(t, ws, "")
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err), "%v", err)
	r.stop(t)
	require.Contains(t, r.out.String(), "create session")
}

func TestPlainServerOffersDirectoriesAndRefusesEmptySessions(t *testing.T) {
	_, home := newWorkspace(t)
	c := newClient(startIn(t, home))

	require.Equal(t, []v1.Workspace{v1.Workspace_WORKSPACE_DIRECTORY}, c.workspaces(t))
	_, err := c.createEmpty(t, "", "req-0123456789abcdef")
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err), "%v", err)
}

func TestServeWithoutSandboxImageNeverTouchesTheGateway(t *testing.T) {
	ws, home := newWorkspace(t)
	api := newFakeAPI(t, textReply("DONE"), 0)
	env := append(apiEnv(api), "XDG_CONFIG_HOME="+t.TempDir())
	c := newClient(startEnv(t, home, env, "--api-token", "dummy"))

	got, err := c.create(t, ws, "")
	require.NoError(t, err)
	require.Equal(t, "DONE", waitDone(t, c, got.GetSession().GetId()).GetResult())
	pins, gerr := filepath.Glob(filepath.Join(home, ".durable", "sessions", "*", "sandbox.json"))
	require.NoError(t, gerr)
	require.Empty(t, pins)
}
