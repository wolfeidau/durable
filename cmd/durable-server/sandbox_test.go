package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/ai-openshell/sdk-go/openshell/v1"
	"github.com/stretchr/testify/require"

	apiv1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/api/durable/v1/durablev1connect"
	"github.com/wolfeidau/durable/internal/fakeapi"
	"github.com/wolfeidau/durable/internal/remote"
)

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
func toolResult(items []*apiv1.WatchEventsResponse, callID string) (string, bool) {
	for _, it := range items {
		m := it.GetEvent().GetMessage()
		if it.GetEvent().GetKind() == apiv1.EventKind_EVENT_KIND_MESSAGE && m.GetRole() == apiv1.Role_ROLE_TOOL && m.GetToolCallId() == callID {
			return m.GetText(), true
		}
	}
	return "", false
}

func TestSandboxRunsToolsInTheSandbox(t *testing.T) {
	image := os.Getenv("DURABLE_SANDBOX_IMAGE")
	if image == "" {
		t.Skip("DURABLE_SANDBOX_IMAGE is not set")
	}
	// The gateway config lives under the real home, and the test's HOME is temporary.
	if os.Getenv("XDG_CONFIG_HOME") == "" {
		realHome, err := os.UserHomeDir()
		require.NoError(t, err)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(realHome, ".config"))
	}
	ws, home := newWorkspace(t)
	l, err := remote.NewSDKLauncher(t.Context())
	if err != nil {
		t.Skipf("no OpenShell gateway: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	pctx, pcancel := context.WithTimeout(t.Context(), 15*time.Second)
	_, err = l.Sandboxes(pctx)
	pcancel()
	if err != nil {
		t.Skipf("OpenShell gateway is not reachable: %v", err)
	}

	id := createSession(t, ws, home, "s")
	name := remote.SandboxName(id)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
		defer cancel()
		_ = l.Delete(ctx, name)
	})

	const command = "echo hello-from-sandbox; sleep 0.5; id -u"
	api := sequenceAPI(t,
		toolReply("toolu_list", "list_files", `{"path":null}`),
		toolReply("toolu_bash1", "bash", bashInput(t, command)),
		textReply("first done"),
		toolReply("toolu_bash2", "bash", bashInput(t, command)),
		textReply("second done"),
	)
	r := startEnv(t, home, apiEnv(api), "--api-token", "dummy", "--idle-timeout", "3s", "--sandbox-image", image)
	c := newClient(r)

	submit := func(reqID, text string) {
		_, err := c.Submit(t.Context(), authed(c, &apiv1.SubmitRequest{SessionId: id, RequestId: reqID, Text: text}))
		require.NoError(t, err)
	}
	watch := func() (*watchStream, context.CancelFunc) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		req := connect.NewRequest(&apiv1.WatchEventsRequest{SessionId: id, IncludeDeltas: true})
		req.Header().Set("Authorization", "Bearer "+r.token)
		s, err := durablev1connect.NewSessionServiceClient(h2cClient(), r.url).WatchEvents(ctx, req)
		require.NoError(t, err)
		return s, func() { cancel(); _ = s.Close() }
	}
	approve := func(seq int64) {
		require.Eventually(t, func() bool {
			_, err := c.decide(t, id, seq, true)
			return connect.CodeOf(err) != connect.CodeNotFound
		}, 3*time.Minute, 100*time.Millisecond)
	}

	stream, closeWatch := watch()
	start := time.Now()
	submit("req-1", "look around, then run it")
	// list_files is read-only and runs without approval, so bash is the first approval.
	approve(1)
	items := lastRun(t, collect(t, stream))
	t.Logf("first run, including sandbox create: %s", time.Since(start))
	closeWatch()

	_, listed := toolResult(items, "toolu_list")
	require.True(t, listed, "list_files ran without approval")
	require.Equal(t, 1, countKind(items, apiv1.EventKind_EVENT_KIND_APPROVAL_REQUESTED))
	out, ok := toolResult(items, "toolu_bash1")
	require.True(t, ok)
	require.Contains(t, out, "hello-from-sandbox")
	require.Contains(t, out, "1500", "the sandbox user, not the host user")
	live := joinText(deltasOf(items), apiv1.DeltaKind_DELTA_KIND_TOOL_OUTPUT)
	require.Contains(t, live, "hello-from-sandbox")

	phase := func() v1.SandboxPhase {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		p, err := l.Phase(ctx, name)
		require.NoError(t, err)
		return p
	}
	require.Equal(t, v1.SandboxReady, phase())
	idleAt := time.Now()
	require.Eventually(t, func() bool { return phase() == v1.SandboxStopped }, time.Minute, 500*time.Millisecond)
	t.Logf("sandbox stopped %s after the run", time.Since(idleAt))

	stream, closeWatch = watch()
	defer closeWatch()
	start = time.Now()
	submit("req-2", "run it again")
	approve(2)
	items = lastRun(t, collectRuns(t, stream, 2))
	t.Logf("second run, including sandbox start: %s", time.Since(start))
	out, ok = toolResult(items, "toolu_bash2")
	require.True(t, ok)
	require.Contains(t, out, "hello-from-sandbox")
	require.Contains(t, out, "1500")
	require.Equal(t, v1.SandboxReady, phase())
}

// collectRuns reads until the stream, which replays from the start, has seen n submissions finish.
func collectRuns(t *testing.T, stream *watchStream, n int) []*apiv1.WatchEventsResponse {
	t.Helper()
	var items []*apiv1.WatchEventsResponse
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

func countKind(items []*apiv1.WatchEventsResponse, kind apiv1.EventKind) int {
	n := 0
	for _, it := range items {
		if it.GetEvent().GetKind() == kind {
			n++
		}
	}
	return n
}
