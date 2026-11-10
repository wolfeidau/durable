package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/ai-openshell/sdk-go/openshell/v1/types"
	"github.com/stretchr/testify/require"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/internal/fakeapi"
	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/sandbox/openshell"
)

// These tests need a running OpenShell gateway and the image from `mise run image:worker`.
const gatewayEnv = "DURABLE_TEST_OPENSHELL"

func toolReply(id, name, input string) string {
	return fakeapi.Stream(apiModel, "tool_use", "null", fakeapi.ToolBlock(0, id, name, input))
}

func bashInput(t *testing.T, command string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"command": command, "timeout_seconds": nil})
	require.NoError(t, err)
	return string(b)
}

// toolResult is the committed result text for a call.
func toolResult(items []*v1.WatchEventsResponse, callID string) (string, bool) {
	for _, it := range items {
		m := it.GetEvent().GetMessage()
		if it.GetEvent().GetKind() == v1.EventKind_EVENT_KIND_MESSAGE && m.GetRole() == v1.Role_ROLE_TOOL && m.GetToolCallId() == callID {
			return m.GetText(), true
		}
	}
	return "", false
}

// collectRuns reads until the stream, which replays from the start, has seen n submissions finish.
func collectRuns(t *testing.T, stream *watchStream, n int) []*v1.WatchEventsResponse {
	t.Helper()
	var items []*v1.WatchEventsResponse
	done := 0
	for stream.Receive() {
		items = append(items, stream.Msg())
		if isDone(stream.Msg()) {
			if done++; done == n {
				return items
			}
		}
	}
	require.NoError(t, stream.Err())
	t.Fatal("stream ended before the submissions finished")
	return nil
}

// gatewayHome returns a workspace and temporary home, and the launcher for the gateway.
func gatewayHome(t *testing.T) (ws, home string, l *openshell.Launcher) {
	t.Helper()
	if os.Getenv(gatewayEnv) != "1" {
		t.Skipf("set %s=1 to run against a gateway", gatewayEnv)
	}
	// The gateway config lives under the real home, and the test's HOME is temporary.
	if os.Getenv("XDG_CONFIG_HOME") == "" {
		realHome, err := os.UserHomeDir()
		require.NoError(t, err)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(realHome, ".config"))
	}
	ws, home = newWorkspace(t)
	l, err := openshell.New(openshell.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	return ws, home, l
}

func gatewayImage() string {
	if img := os.Getenv("DURABLE_TEST_OPENSHELL_IMAGE"); img != "" {
		return img
	}
	return "durable-worker:dev"
}

// sessionPin reads the session's sandbox pin and deletes the sandbox at cleanup.
func sessionPin(t *testing.T, home, id string, l *openshell.Launcher) sandbox.Pin {
	t.Helper()
	pin, ok, err := sandbox.ReadPin(filepath.Join(home, ".durable", "sessions", id))
	require.NoError(t, err)
	require.True(t, ok, "the session has a sandbox pin")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Minute)
		defer cancel()
		_ = l.Delete(ctx, pin.Name)
	})
	return pin
}

func phaseOf(t *testing.T, l *openshell.Launcher, name string) (types.SandboxPhase, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	return l.Phase(ctx, name)
}

// approveSeq decides approval seq once the run has parked on it.
func approveSeq(t *testing.T, c apiClient, id string, seq int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		_, err := c.decide(t, id, seq, true)
		return connect.CodeOf(err) != connect.CodeNotFound
	}, 3*time.Minute, 100*time.Millisecond)
}

func TestGatewaySessionRunsToolsInSandboxStopsIdleAndKeepsWorkspace(t *testing.T) {
	ws, home, l := gatewayHome(t)
	const command = "echo hello-from-sandbox; echo kept > /sandbox/kept.txt; sleep 0.5; id -u"
	api := sequenceAPI(t,
		toolReply("toolu_list", "list_files", `{"path":null}`),
		toolReply("toolu_bash1", "bash", bashInput(t, command)),
		textReply("first done"),
		toolReply("toolu_bash2", "bash", bashInput(t, "cat /sandbox/kept.txt")),
		textReply("second done"),
	)
	r := startEnv(t, home, apiEnv(api), "--api-token", "dummy", "--idle-timeout", "2s", "--sandbox-image", gatewayImage())
	c := newClient(r)

	got, err := c.create(t, ws, "")
	require.NoError(t, err)
	id := got.GetSession().GetId()
	pin := sessionPin(t, home, id, l)

	stream := openWatch(t, r, id, true)
	submitAs(t, c, id, "look around, then run it")
	// list_files is read-only and runs without approval, so bash is the first approval.
	approveSeq(t, c, id, 1)
	items := lastRun(t, collectRuns(t, stream, 1))

	_, listed := toolResult(items, "toolu_list")
	require.True(t, listed, "list_files ran without approval")
	out, ok := toolResult(items, "toolu_bash1")
	require.True(t, ok)
	require.Contains(t, out, "hello-from-sandbox")
	require.Contains(t, out, "1500", "the sandbox user, not the host user")
	require.Contains(t, joinText(deltasOf(items), v1.DeltaKind_DELTA_KIND_TOOL_OUTPUT), "hello-from-sandbox")
	require.NoFileExists(t, filepath.Join(ws, "kept.txt"))
	// An open watch holds the session, so the idle stop waits for it.
	require.NoError(t, stream.Close())

	require.Eventually(t, func() bool {
		p, err := phaseOf(t, l, pin.Name)
		return err == nil && p == types.SandboxStopped
	}, time.Minute, 500*time.Millisecond)

	stream = openWatch(t, r, id, true)
	_, err = c.Submit(t.Context(), authed(c, &v1.SubmitRequest{SessionId: id, RequestId: "req-2", Text: "read it back"}))
	require.NoError(t, err)
	approveSeq(t, c, id, 2)
	items = lastRun(t, collectRuns(t, stream, 2))
	out, ok = toolResult(items, "toolu_bash2")
	require.True(t, ok)
	require.Contains(t, out, "kept", "the workspace survived the stop")
	p, err := phaseOf(t, l, pin.Name)
	require.NoError(t, err)
	require.Equal(t, types.SandboxReady, p)
}

func TestGatewaySandboxDeleteCreatesAFreshSandboxOnNextOpen(t *testing.T) {
	ws, home, l := gatewayHome(t)
	api := sequenceAPI(t,
		toolReply("toolu_bash1", "bash", bashInput(t, "echo kept > /sandbox/kept.txt")),
		textReply("first done"),
	)
	r := startEnv(t, home, apiEnv(api), "--api-token", "dummy", "--sandbox-image", gatewayImage())
	c := newClient(r)
	got, err := c.create(t, ws, "")
	require.NoError(t, err)
	id := got.GetSession().GetId()
	pin := sessionPin(t, home, id, l)

	stream := openWatch(t, r, id, false)
	submitAs(t, c, id, "write it")
	approveSeq(t, c, id, 1)
	collectRuns(t, stream, 1)
	r.stop(t)

	out, err := sandboxDelete(t, home, id)
	require.NoError(t, err, out)
	_, err = phaseOf(t, l, pin.Name)
	require.ErrorIs(t, err, openshell.ErrNotFound)
	again, ok, err := sandbox.ReadPin(filepath.Join(home, ".durable", "sessions", id))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, pin, again, "the pin is kept")

	api = sequenceAPI(t,
		toolReply("toolu_bash2", "bash", bashInput(t, "ls /sandbox/kept.txt || echo fresh")),
		textReply("second done"),
	)
	r = startEnv(t, home, apiEnv(api), "--api-token", "dummy")
	c = newClient(r)
	stream = openWatch(t, r, id, false)
	_, err = c.Submit(t.Context(), authed(c, &v1.SubmitRequest{SessionId: id, RequestId: "req-2", Text: "look"}))
	require.NoError(t, err)
	approveSeq(t, c, id, 2)
	items := lastRun(t, collectRuns(t, stream, 2))
	res, ok := toolResult(items, "toolu_bash2")
	require.True(t, ok)
	require.Contains(t, res, "fresh")
	p, err := phaseOf(t, l, pin.Name)
	require.NoError(t, err)
	require.Equal(t, types.SandboxReady, p)
}
