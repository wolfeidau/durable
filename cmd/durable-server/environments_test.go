package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/internal/commands"
	"github.com/wolfeidau/durable/internal/sandbox"
)

const goEnvironments = `{"go": {"image": "durable-worker-go:dev", "grants": ["go-modules"]}}`

func writeEnvironments(t *testing.T, home, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".durable", "environments.json"), []byte(body), 0o600))
}

// envServer starts a sandboxed server with no reachable gateway.
func envServer(t *testing.T, home string, args ...string) *running {
	t.Helper()
	api := newFakeAPI(t, textReply("DONE"), 0)
	env := append(apiEnv(api), "XDG_CONFIG_HOME="+t.TempDir(), "DURABLE_GITHUB_TOKEN=")
	return startEnv(t, home, env, append([]string{"--api-token", "dummy"}, args...)...)
}

func (c apiClient) createEmptyIn(t *testing.T, environment, requestID string) (*v1.CreateSessionResponse, error) {
	t.Helper()
	resp, err := c.CreateSession(t.Context(), authed(c, &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_EMPTY, RequestId: requestID, Environment: environment}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func TestServerInfoListsEnvironmentsFromFileAndFlag(t *testing.T) {
	_, home := newWorkspace(t)
	writeEnvironments(t, home, goEnvironments)
	c := newClient(envServer(t, home, "--sandbox-image", "durable-worker:dev"))

	resp, err := c.GetServerInfo(t.Context(), authed(c, &v1.GetServerInfoRequest{}))

	require.NoError(t, err)
	require.Equal(t, []v1.Workspace{v1.Workspace_WORKSPACE_EMPTY, v1.Workspace_WORKSPACE_REPO}, resp.Msg.GetWorkspaces())
	envs := resp.Msg.GetEnvironments()
	require.Len(t, envs, 2)
	require.Equal(t, "default", envs[0].GetName())
	require.Equal(t, "durable-worker:dev", envs[0].GetImage())
	require.Empty(t, envs[0].GetGrants())
	require.Equal(t, "go", envs[1].GetName())
	require.Equal(t, "durable-worker-go:dev", envs[1].GetImage())
	require.Equal(t, []string{"go-modules"}, envs[1].GetGrants())
}

func TestEnvironmentsFileAloneMakesTheServerSandboxed(t *testing.T) {
	_, home := newWorkspace(t)
	writeEnvironments(t, home, goEnvironments)
	c := newClient(envServer(t, home))

	require.Equal(t, []v1.Workspace{v1.Workspace_WORKSPACE_EMPTY, v1.Workspace_WORKSPACE_REPO}, c.workspaces(t))
}

func TestCreateWithBadEnvironmentIsInvalidArgument(t *testing.T) {
	ws, home := newWorkspace(t)
	writeEnvironments(t, home, `{"go": {"image": "img"}, "rust": {"image": "img"}}`)
	c := newClient(envServer(t, home))

	_, err := c.createEmptyIn(t, "nope", "req-env-0123456789")
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "%v", err)
	require.ErrorContains(t, err, "go, rust")

	_, err = c.createEmptyIn(t, "", "req-env-0123456789")
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "two environments and no default is ambiguous: %v", err)

	_, err = c.CreateSession(t.Context(), authed(c, &v1.CreateSessionRequest{Cwd: ws, Environment: "go"}))
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "a directory workspace takes no environment: %v", err)

	_, err = c.CreateSession(t.Context(), authed(c, &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_REPO, Repo: "github.com/octocat/hello-world", Branch: "main", Environment: "nope"}))
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "%v", err)
}

func TestPlainServerRejectsAnEnvironment(t *testing.T) {
	_, home := newWorkspace(t)
	c := newClient(startIn(t, home))

	_, err := c.createEmptyIn(t, "go", "req-env-0123456789")

	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "%v", err)
}

func TestInvalidEnvironmentsFileFailsStartup(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		args []string
		want string
	}{
		"unknown grant": {`{"go": {"image": "img", "grants": ["nope"]}}`, nil, "unknown grant"},
		"empty image":   {`{"go": {"image": ""}}`, nil, "image is required"},
		"unknown field": {`{"go": {"image": "img", "egress": []}}`, nil, "unknown field"},
		"default clash": {`{"default": {"image": "img"}}`, []string{"--sandbox-image", "durable-worker:dev"}, "which ~/.durable/environments.json also defines"},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			writeEnvironments(t, home, tc.body)

			out := runFailingIn(t, home, nil, tc.args...)

			require.Contains(t, out, tc.want)
			require.Contains(t, out, "environments.json")
		})
	}
}

func TestCreateWithEnvironmentPinsImageAndGrants(t *testing.T) {
	_, home := newWorkspace(t)
	writeEnvironments(t, home, goEnvironments)
	c := newClient(envServer(t, home))

	_, err := c.createEmptyIn(t, "go", "req-env-0123456789")

	// There is no gateway, so the create fails after the pin is written.
	require.Equal(t, connect.CodeUnavailable, connect.CodeOf(err), "%v", err)
	pins, gerr := filepath.Glob(filepath.Join(home, ".durable", "sessions", "*", "sandbox.json"))
	require.NoError(t, gerr)
	require.Len(t, pins, 1)
	pin, ok, err := sandbox.ReadPin(filepath.Dir(pins[0]))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "durable-worker-go:dev", pin.Image)
	require.Equal(t, []sandbox.Grant{{
		Name:      "go-modules",
		Binaries:  []string{"/usr/local/go/bin/go"},
		Endpoints: []sandbox.GrantEndpoint{{Host: "proxy.golang.org", Port: 443}, {Host: "sum.golang.org", Port: 443}},
	}}, pin.Grants)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(pins[0]), "session.json"))
	require.NoError(t, err)
	var meta struct{ Environment string }
	require.NoError(t, json.Unmarshal(raw, &meta))
	require.Equal(t, "go", meta.Environment)
}

func TestRepeatedCreateReturnsTheFirstSessionEvenWhenTheEnvironmentChanged(t *testing.T) {
	_, home := newWorkspace(t)
	writeEnvironments(t, home, `{"go": {"image": "durable-worker-go:dev"}, "rust": {"image": "durable-worker-rust:dev"}}`)
	r := envServer(t, home)
	c := newClient(r)
	const requestID = "req-repeat-0123456789"

	// The session as a first create leaves it, written directly because there is no gateway to open its sandbox.
	dir := filepath.Join(home, ".durable", "sessions", commands.EmptySessionID(requestID))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, commands.WriteSessionMeta(dir, commands.SessionMeta{Name: "s", Workspace: commands.WorkspaceEmpty, Environment: "go", CreatedAt: time.Now().UTC()}))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "session.sqlite"), nil, 0o600))
	_, err := sandbox.WritePin(dir, "durable-worker-go:dev")
	require.NoError(t, err)
	pinPath := filepath.Join(dir, sandbox.PinFile)
	before, err := os.ReadFile(pinPath)
	require.NoError(t, err)

	got, err := c.createEmptyIn(t, "rust", requestID)

	require.NoError(t, err)
	require.False(t, got.GetCreated())
	require.Equal(t, "go", got.GetSession().GetEnvironment())
	after, err := os.ReadFile(pinPath)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
	require.Contains(t, string(after), "durable-worker-go:dev")

	// A restart whose file no longer has the environment still returns the session.
	require.NoError(t, r.cmd.Process.Kill())
	<-r.done
	require.NoError(t, os.Remove(filepath.Join(home, ".durable", "server.json")))
	writeEnvironments(t, home, `{"rust": {"image": "durable-worker-rust:dev"}, "node": {"image": "durable-worker-node:dev"}}`)
	c = newClient(envServer(t, home))

	got, err = c.createEmptyIn(t, "go", requestID)

	require.NoError(t, err)
	require.False(t, got.GetCreated())
	require.Equal(t, "go", got.GetSession().GetEnvironment())
	got, err = c.createEmptyIn(t, "", requestID)
	require.NoError(t, err, "an empty name is ambiguous for a new session, not an existing one")
	require.Equal(t, "go", got.GetSession().GetEnvironment())
}

func TestListSessionsReportsTheEnvironment(t *testing.T) {
	_, home := newWorkspace(t)
	dir := filepath.Join(home, ".durable", "sessions", "0123456789abcdef")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, commands.WriteSessionMeta(dir, commands.SessionMeta{Name: "s", Workspace: commands.WorkspaceEmpty, Environment: "go", CreatedAt: time.Now().UTC()}))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "session.sqlite"), nil, 0o600))
	older := filepath.Join(home, ".durable", "sessions", "fedcba9876543210")
	require.NoError(t, os.MkdirAll(older, 0o700))
	require.NoError(t, commands.WriteSessionMeta(older, commands.SessionMeta{Name: "old", Workspace: commands.WorkspaceEmpty, CreatedAt: time.Now().UTC()}))
	require.NoError(t, os.WriteFile(filepath.Join(older, "session.sqlite"), nil, 0o600))
	c := newClient(startIn(t, home))

	list, err := c.ListSessions(t.Context(), authed(c, &v1.ListSessionsRequest{}))

	require.NoError(t, err)
	got := map[string]string{}
	for _, s := range list.Msg.GetSessions() {
		got[s.GetName()] = s.GetEnvironment()
	}
	require.Equal(t, map[string]string{"s": "go", "old": ""}, got)
}
