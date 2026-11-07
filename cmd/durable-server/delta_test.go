package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/api/durable/v1/durablev1connect"
	"github.com/wolfeidau/durable/internal/fakeapi"
)

type watchStream = connect.ServerStreamForClient[v1.WatchEventsResponse]

// openWatch opens a watch from seq 0. A watch returns its headers with its first item, so the session needs an event first.
func openWatch(t *testing.T, r *running, id string, includeDeltas bool) *watchStream {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), watchTimeout)
	client := durablev1connect.NewSessionServiceClient(h2cClient(), r.url)
	req := connect.NewRequest(&v1.WatchEventsRequest{SessionId: id, IncludeDeltas: includeDeltas})
	req.Header().Set("Authorization", "Bearer "+r.token)
	stream, err := client.WatchEvents(ctx, req)
	require.NoError(t, err)
	// Cleanups run last in, first out, so the cancel precedes Close, which would otherwise wait out a heartbeat.
	t.Cleanup(func() { _ = stream.Close() })
	t.Cleanup(cancel)
	return stream
}

// collect reads items until the stream has seen a submission finish.
func collect(t *testing.T, stream *watchStream) []*v1.WatchEventsResponse {
	t.Helper()
	var items []*v1.WatchEventsResponse
	for stream.Receive() {
		items = append(items, stream.Msg())
		if isDone(stream.Msg()) {
			return items
		}
	}
	require.NoError(t, stream.Err())
	t.Fatal("stream ended before the submission finished")
	return nil
}

func isDone(it *v1.WatchEventsResponse) bool {
	e := it.GetEvent()
	return e.GetKind() == v1.EventKind_EVENT_KIND_SUBMISSION && e.GetStatus() == v1.SubmissionStatus_SUBMISSION_STATUS_DONE
}

func isQueued(it *v1.WatchEventsResponse) bool {
	e := it.GetEvent()
	return e.GetKind() == v1.EventKind_EVENT_KIND_SUBMISSION && e.GetStatus() == v1.SubmissionStatus_SUBMISSION_STATUS_QUEUED
}

func isAssistantMessage(it *v1.WatchEventsResponse) bool {
	e := it.GetEvent()
	return e.GetKind() == v1.EventKind_EVENT_KIND_MESSAGE && e.GetMessage().GetRole() == v1.Role_ROLE_ASSISTANT
}

func isCallDone(it *v1.WatchEventsResponse) bool {
	return it.GetEvent().GetKind() == v1.EventKind_EVENT_KIND_CALL_DONE
}

func indexOf(items []*v1.WatchEventsResponse, match func(*v1.WatchEventsResponse) bool) int {
	for i, it := range items {
		if match(it) {
			return i
		}
	}
	return -1
}

// lastRun is the items from the newest submission on.
func lastRun(t *testing.T, items []*v1.WatchEventsResponse) []*v1.WatchEventsResponse {
	t.Helper()
	for i := range slices.Backward(items) {
		if isQueued(items[i]) {
			return items[i:]
		}
	}
	t.Fatal("no submission in the stream")
	return nil
}

func deltasOf(items []*v1.WatchEventsResponse) []*v1.Delta {
	var out []*v1.Delta
	for _, it := range items {
		if d := it.GetDelta(); d != nil {
			out = append(out, d)
		}
	}
	return out
}

func joinText(deltas []*v1.Delta, kind v1.DeltaKind) string {
	var b strings.Builder
	for _, d := range deltas {
		if d.GetKind() == kind {
			b.WriteString(d.GetText())
		}
	}
	return b.String()
}

func chunkedReply(parts ...string) string {
	return fakeapi.Stream(apiModel, "end_turn", "null", fakeapi.TextBlock(0, parts...))
}

// sequenceAPI answers request n with replies[n], and any later request with end_turn text.
// It ignores the request body, so nil and empty inputs are fine.
func sequenceAPI(t *testing.T, replies ...string) *fakeAPI {
	t.Helper()
	var n atomic.Int64
	f := &fakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		i := int(n.Add(1)) - 1
		w.Header().Set("Content-Type", "text/event-stream")
		if i < len(replies) {
			_, _ = io.WriteString(w, replies[i])
			return
		}
		_, _ = io.WriteString(w, textReply("all done"))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func submitAs(t *testing.T, c apiClient, id, text string) {
	t.Helper()
	_, err := c.Submit(t.Context(), authed(c, &v1.SubmitRequest{SessionId: id, RequestId: "req-1", Text: text}))
	require.NoError(t, err)
}

// startDeltaServer serves a fresh session. Tests open a watch and submit at once, as a client would.
func startDeltaServer(t *testing.T, replies ...string) (c apiClient, r *running, id string) {
	t.Helper()
	ws, home := newWorkspace(t)
	id = createSession(t, ws, home, "s")
	r = startEnv(t, home, apiEnv(sequenceAPI(t, replies...)), "--api-token", "dummy")
	return newClient(r), r, id
}

func TestWatchDeltasPrecedeTheMessage(t *testing.T) {
	c, r, id := startDeltaServer(t, chunkedReply("hello", " wide", " world"))
	stream := openWatch(t, r, id, true)
	submitAs(t, c, id, "hi")
	items := lastRun(t, collect(t, stream))

	msg := indexOf(items, isAssistantMessage)
	require.GreaterOrEqual(t, msg, 0)
	before := deltasOf(items[:msg])
	require.NotEmpty(t, before)
	require.Equal(t, v1.DeltaKind_DELTA_KIND_RESET, before[0].GetKind())
	require.Equal(t, items[msg].GetEvent().GetSubmission(), before[0].GetSubmission())
	require.Equal(t, "hello wide world", joinText(before, v1.DeltaKind_DELTA_KIND_TEXT))
	require.Equal(t, "hello wide world", items[msg].GetEvent().GetMessage().GetText())
	for _, d := range before {
		require.LessOrEqual(t, d.GetLastSeq(), items[msg].GetEvent().GetSeq())
	}
}

func TestWatchWithoutIncludeDeltasSendsNone(t *testing.T) {
	c, r, id := startDeltaServer(t, chunkedReply("hello", " world"))
	stream := openWatch(t, r, id, false)
	submitAs(t, c, id, "hi")
	items := collect(t, stream)
	require.Empty(t, deltasOf(items))
	require.GreaterOrEqual(t, indexOf(items, isAssistantMessage), 0)
}

// approveFirst decides approval 1 once the run has parked on it.
func approveFirst(t *testing.T, c apiClient, id string) {
	t.Helper()
	require.Eventually(t, func() bool {
		_, err := c.decide(t, id, 1, true)
		return connect.CodeOf(err) != connect.CodeNotFound
	}, testTimeout, 20*time.Millisecond)
}

const slowCommand = "echo one; sleep 0.3; echo two"

func TestWatchToolOutputDeltasPrecedeCallDone(t *testing.T) {
	c, r, id := startDeltaServer(t, bashReply(slowCommand))
	stream := openWatch(t, r, id, true)
	submitAs(t, c, id, "run it")
	approveFirst(t, c, id)
	items := lastRun(t, collect(t, stream))

	callDone := indexOf(items, isCallDone)
	require.GreaterOrEqual(t, callDone, 0)
	var out []*v1.Delta
	for _, d := range deltasOf(items[:callDone]) {
		if d.GetKind() == v1.DeltaKind_DELTA_KIND_TOOL_OUTPUT {
			require.Equal(t, "toolu_1", d.GetCallId())
			out = append(out, d)
		}
	}
	require.NotEmpty(t, out)
	text := joinText(out, v1.DeltaKind_DELTA_KIND_TOOL_OUTPUT)
	require.Contains(t, text, "one")
	require.Contains(t, text, "two")
}

func TestPostToolUseHookWithholdsToolOutputDeltas(t *testing.T) {
	ws, home := newWorkspace(t)
	script := filepath.Join(t.TempDir(), "hook.sh")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/bash\ncat >/dev/null\n"), 0o700))
	cfg, err := json.Marshal(map[string]any{"hooks": map[string]any{"PostToolUse": []any{map[string]any{
		"matcher": "bash",
		"hooks":   []any{map[string]any{"type": "command", "command": script}},
	}}}})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".durable", "hooks.json"), cfg, 0o600))

	id := createSession(t, ws, home, "s")
	r := startEnv(t, home, apiEnv(sequenceAPI(t, bashReply(slowCommand))), "--api-token", "dummy")
	c := newClient(r)
	stream := openWatch(t, r, id, true)
	submitAs(t, c, id, "run it")
	approveFirst(t, c, id)
	items := lastRun(t, collect(t, stream))

	require.GreaterOrEqual(t, indexOf(items, isCallDone), 0)
	require.NotEmpty(t, deltasOf(items), "the reset still arrives")
	require.Empty(t, joinText(deltasOf(items), v1.DeltaKind_DELTA_KIND_TOOL_OUTPUT))
}

func TestStalledWatcherDoesNotBlockTheRunOrOthers(t *testing.T) {
	// Several MB of deltas overflow the stalled stream's flow-control window and the per-watcher buffer.
	chunk := strings.Repeat("x", 1500)
	parts := make([]string, 3000)
	for i := range parts {
		parts[i] = chunk
	}
	c, r, id := startDeltaServer(t, chunkedReply(parts...))

	stalled := openWatch(t, r, id, true)
	_ = stalled // never read
	reader := openWatch(t, r, id, true)
	submitAs(t, c, id, "hi")

	start := time.Now()
	items := lastRun(t, collect(t, reader))
	require.Less(t, time.Since(start), 30*time.Second)
	msg := indexOf(items, isAssistantMessage)
	require.GreaterOrEqual(t, msg, 0)
	require.Len(t, items[msg].GetEvent().GetMessage().GetText(), len(chunk)*len(parts))
}
