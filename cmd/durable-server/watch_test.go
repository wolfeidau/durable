package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/wolfeidau/durable"
	v1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/api/durable/v1/durablev1connect"
	"github.com/wolfeidau/durable/hooks"
)

const watchTimeout = 90 * time.Second

// watcher follows one session across reconnects, resuming from what it has seen.
type watcher struct {
	client     durablev1connect.SessionServiceClient
	token, id  string
	events     []*v1.Event
	heartbeats int
}

func newWatcher(r *running, id string, opts ...connect.ClientOption) *watcher {
	return &watcher{client: durablev1connect.NewSessionServiceClient(h2cClient(), r.url, opts...), token: r.token, id: id}
}

func (w *watcher) last() int64 {
	if len(w.events) == 0 {
		return 0
	}
	return w.events[len(w.events)-1].GetSeq()
}

func (w *watcher) epoch() string {
	if len(w.events) == 0 {
		return ""
	}
	return w.events[0].GetEpoch()
}

func (w *watcher) done() int {
	n := 0
	for _, e := range w.events {
		if e.GetKind() == v1.EventKind_EVENT_KIND_SUBMISSION && e.GetStatus() == v1.SubmissionStatus_SUBMISSION_STATUS_DONE {
			n++
		}
	}
	return n
}

func (w *watcher) request(epoch string, after int64) *connect.Request[v1.WatchEventsRequest] {
	req := connect.NewRequest(&v1.WatchEventsRequest{SessionId: w.id, Epoch: epoch, AfterSeq: after})
	req.Header().Set("Authorization", "Bearer "+w.token)
	return req
}

// tryFollow streams from the watcher's cursor until stop returns true, then disconnects.
func (w *watcher) tryFollow(stop func() bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), watchTimeout)
	defer cancel()
	stream, err := w.client.WatchEvents(ctx, w.request(w.epoch(), w.last()))
	if err != nil {
		return err
	}
	defer stream.Close()
	for !stop() {
		if !stream.Receive() {
			return fmt.Errorf("stream ended early: %w", stream.Err())
		}
		switch item := stream.Msg().GetItem().(type) {
		case *v1.WatchEventsResponse_Event:
			w.events = append(w.events, item.Event)
		case *v1.WatchEventsResponse_Heartbeat:
			w.heartbeats++
		}
	}
	return nil
}

func (w *watcher) follow(t *testing.T, stop func() bool) {
	t.Helper()
	require.NoError(t, w.tryFollow(stop))
}

func (w *watcher) followUntilDone(t *testing.T, submissions int) {
	t.Helper()
	w.follow(t, func() bool { return w.done() >= submissions })
}

func seqs(events []*v1.Event) []int64 {
	out := make([]int64, 0, len(events))
	for _, e := range events {
		out = append(out, e.GetSeq())
	}
	return out
}

func storedSeqs(events []durable.Event) []int64 {
	out := make([]int64, 0, len(events))
	for _, e := range events {
		out = append(out, e.Seq)
	}
	return out
}

// requireContiguous checks seqs run 1..n with no gap or duplicate.
func requireContiguous(t *testing.T, events []*v1.Event) {
	t.Helper()
	for i, e := range events {
		require.Equal(t, int64(i+1), e.GetSeq(), "seqs: %v", seqs(events))
	}
}

// stop ends the server with SIGTERM and waits, so the session lease is released.
func (r *running) stop(t *testing.T) {
	t.Helper()
	require.NoError(t, r.cmd.Process.Signal(syscall.SIGTERM))
	select {
	case <-r.done:
	case <-time.After(30 * time.Second):
		t.Fatal("server did not exit after SIGTERM")
	}
}

// crash kills the server without cleanup and clears its stale server.json, so a restart reads its own.
func (r *running) crash(t *testing.T) {
	t.Helper()
	require.NoError(t, r.cmd.Process.Kill())
	<-r.done
	require.NoError(t, os.Remove(filepath.Join(r.home, ".durable", "server.json")))
}

// seedSession creates a session with queued submissions and returns its ID and database path.
func seedSession(t *testing.T, reply string, delay time.Duration, submissions int) (ws, home, id, dbPath string) {
	t.Helper()
	ws, home = newWorkspace(t)
	useAPI(t, newFakeAPI(t, reply, delay))
	// Empty stdin creates the session without calling the model.
	chatOnce(t, ws, "")
	id, dbPath = sessionDB(t, home)
	texts := make([]string, submissions)
	for i := range texts {
		texts[i] = "go"
	}
	queue(t, dbPath, texts...)
	return ws, home, id, dbPath
}

func TestWatchContiguousAcrossClientsAndProtocols(t *testing.T) {
	for name, opts := range map[string][]connect.ClientOption{
		"connect":  nil,
		"grpc":     {connect.WithGRPC()},
		"grpc-web": {connect.WithGRPCWeb()},
	} {
		t.Run(name, func(t *testing.T) {
			_, home, id, dbPath := seedSession(t, textReply("hi"), 150*time.Millisecond, 2)
			r := startIn(t, home)

			watchers := make([]*watcher, 3)
			errs := make([]error, len(watchers))
			var wg sync.WaitGroup
			for i := range watchers {
				watchers[i] = newWatcher(r, id, opts...)
				wg.Go(func() {
					// Clients attach at different points in the run.
					time.Sleep(time.Duration(i) * 200 * time.Millisecond)
					errs[i] = watchers[i].tryFollow(func() bool { return watchers[i].done() >= 2 })
				})
			}
			wg.Wait()
			require.NoError(t, errors.Join(errs...))
			r.stop(t)

			want := storedSeqs(allEvents(t, dbPath))
			for _, w := range watchers {
				requireContiguous(t, w.events)
				require.Equal(t, want, seqs(w.events))
			}
		})
	}
}

func TestWatchResumesWithoutGapsOrDuplicates(t *testing.T) {
	_, home, id, dbPath := seedSession(t, textReply("hi"), 150*time.Millisecond, 2)
	r := startIn(t, home)

	w := newWatcher(r, id)
	w.follow(t, func() bool { return len(w.events) >= 4 })
	first := w.last()
	w.followUntilDone(t, 2)
	require.Greater(t, w.last(), first)
	r.stop(t)

	requireContiguous(t, w.events)
	require.Equal(t, storedSeqs(allEvents(t, dbPath)), seqs(w.events))
}

func TestWatchResumesAfterServerCrash(t *testing.T) {
	_, home, id, dbPath := seedSession(t, textReply("hi"), 400*time.Millisecond, 2)
	r := startIn(t, home, "--open-timeout", "60s")

	w := newWatcher(r, id)
	w.follow(t, func() bool { return len(w.events) >= 4 })
	epoch := w.epoch()
	r.crash(t)

	// The restart waits for the dead server's lease to expire.
	r = startIn(t, home, "--open-timeout", "60s")
	w.client = newWatcher(r, id).client
	w.token = r.token
	w.followUntilDone(t, 2)
	r.stop(t)

	require.Equal(t, epoch, w.events[len(w.events)-1].GetEpoch())
	requireContiguous(t, w.events)
	require.Equal(t, storedSeqs(allEvents(t, dbPath)), seqs(w.events))
}

// epochMismatchOf returns the detail of a FAILED_PRECONDITION error.
func epochMismatchOf(t *testing.T, err error) *v1.EpochMismatch {
	t.Helper()
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err), "%v", err)
	var cerr *connect.Error
	require.ErrorAs(t, err, &cerr)
	for _, d := range cerr.Details() {
		msg, verr := d.Value()
		require.NoError(t, verr)
		if m, ok := msg.(*v1.EpochMismatch); ok {
			return m
		}
	}
	t.Fatal("no EpochMismatch detail")
	return nil
}

func watchError(t *testing.T, w *watcher, epoch string, after int64) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), watchTimeout)
	defer cancel()
	stream, err := w.client.WatchEvents(ctx, w.request(epoch, after))
	require.NoError(t, err)
	defer stream.Close()
	require.False(t, stream.Receive(), "expected the stream to fail")
	return stream.Err()
}

func TestWatchRejectsStaleEpochAndFutureSeq(t *testing.T) {
	_, home, id, _ := seedSession(t, textReply("hi"), 0, 1)
	r := startIn(t, home)

	w := newWatcher(r, id)
	w.followUntilDone(t, 1)
	last := w.last()

	m := epochMismatchOf(t, watchError(t, w, "not-the-epoch", 0))
	require.Equal(t, w.epoch(), m.GetEpoch())
	require.Equal(t, last, m.GetLastSeq())

	// A cursor with no epoch can't be trusted, so it replays from 0 too.
	m = epochMismatchOf(t, watchError(t, w, "", 1))
	require.Equal(t, w.epoch(), m.GetEpoch())

	m = epochMismatchOf(t, watchError(t, w, w.epoch(), last+1))
	require.Equal(t, w.epoch(), m.GetEpoch())
	require.Equal(t, last, m.GetLastSeq())
}

func TestWatchAfterRestoreReplaysUnderNewEpoch(t *testing.T) {
	ws, home, id, dbPath := seedSession(t, textReply("hi"), 0, 1)
	r := startIn(t, home)
	old := newWatcher(r, id)
	old.followUntilDone(t, 1)
	r.stop(t)

	snap := filepath.Join(t.TempDir(), "snap.sqlite")
	h := openSession(t, dbPath)
	require.NoError(t, h.Snapshot(t.Context(), snap))
	require.NoError(t, h.Close())

	// More history after the snapshot, so the old cursor lies past the restored log.
	chatOnce(t, ws, "more\n")
	require.NoError(t, durable.Restore(t.Context(), snap, dbPath))

	// One new event under the new epoch.
	chatOnce(t, ws, "after restore\n")
	newEvents := allEvents(t, dbPath)

	r = startIn(t, home)
	old.client, old.token = newWatcher(r, id).client, r.token
	m := epochMismatchOf(t, watchError(t, old, old.epoch(), old.last()))
	require.NotEqual(t, old.epoch(), m.GetEpoch())

	fresh := newWatcher(r, id)
	fresh.follow(t, func() bool { return fresh.last() >= newEvents[len(newEvents)-1].Seq })
	require.Equal(t, m.GetEpoch(), fresh.epoch())
	requireContiguous(t, fresh.events)
	require.Equal(t, storedSeqs(newEvents), seqs(fresh.events))
	r.stop(t)
}

func TestWatchSendsHeartbeatsWhenIdle(t *testing.T) {
	_, home, id, _ := seedSession(t, textReply("hi"), 0, 1)
	r := startIn(t, home, "--heartbeat", "150ms")

	w := newWatcher(r, id)
	w.followUntilDone(t, 1)
	start := time.Now()
	w.follow(t, func() bool { return w.heartbeats >= 3 })
	elapsed := time.Since(start)
	require.GreaterOrEqual(t, elapsed, 300*time.Millisecond)
	require.Less(t, elapsed, 5*time.Second)
	r.stop(t)
}

func TestWatchHoldsSessionAndIdleCloseReleasesIt(t *testing.T) {
	_, home, id, dbPath := seedSession(t, textReply("hi"), 0, 1)
	r := startIn(t, home, "--idle-timeout", "300ms")

	w := newWatcher(r, id)
	ctx, cancel := context.WithCancel(t.Context())
	stream, err := w.client.WatchEvents(ctx, w.request("", 0))
	require.NoError(t, err)
	for w.done() < 1 {
		require.True(t, stream.Receive(), "%v", stream.Err())
		if e := stream.Msg().GetEvent(); e != nil {
			w.events = append(w.events, e)
		}
	}

	// Well past the idle timeout, the open stream still holds the session.
	time.Sleep(time.Second)
	probe, cancelProbe := context.WithTimeout(t.Context(), 300*time.Millisecond)
	_, err = durable.Open(probe, dbPath, durable.Options{Model: idleModel{}})
	cancelProbe()
	require.Error(t, err, "session closed while a stream held it")

	cancel()
	_ = stream.Close()

	require.Eventually(t, func() bool {
		probe, cancelProbe := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancelProbe()
		h, err := durable.Open(probe, dbPath, durable.Options{Model: idleModel{}})
		if err != nil {
			return false
		}
		_ = h.Close()
		return true
	}, 15*time.Second, 50*time.Millisecond, "idle session kept its lease")

	// A new watch reopens it under the same epoch.
	again := newWatcher(r, id)
	again.followUntilDone(t, 1)
	require.Equal(t, w.epoch(), again.epoch())
	require.Equal(t, seqs(w.events), seqs(again.events))
	r.stop(t)
}

func TestWatchWaitsOutAHeldLeaseThenReports(t *testing.T) {
	_, home, id, dbPath := seedSession(t, textReply("hi"), 0, 1)
	r := startIn(t, home, "--open-timeout", "300ms")

	h := openSession(t, dbPath)
	defer h.Close()

	w := newWatcher(r, id)
	ctx, cancel := context.WithTimeout(t.Context(), watchTimeout)
	defer cancel()
	stream, err := w.client.WatchEvents(ctx, w.request("", 0))
	require.NoError(t, err)
	defer stream.Close()
	require.False(t, stream.Receive())
	require.Equal(t, connect.CodeUnavailable, connect.CodeOf(stream.Err()), "%v", stream.Err())
}

func TestWatchNeverPutsARewrittenCallInputOnTheWire(t *testing.T) {
	const marker = "REWRITTEN-MARKER"
	ws, home := newWorkspace(t)
	rewrite := filepath.Join(t.TempDir(), "hook.sh")
	body := "#!/bin/bash\necho '{\"hookSpecificOutput\":{\"updatedInput\":{\"command\":\"true # " + marker + "\"}}}'\n"
	require.NoError(t, os.WriteFile(rewrite, []byte(body), 0o700))
	hookCfg := `{"hooks":{"` + hooks.PreToolUse + `":[{"matcher":"bash","hooks":[{"type":"command","command":"` + rewrite + `"}]}]}}`
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".durable", "hooks.json"), []byte(hookCfg), 0o600))

	api := newFakeAPI(t, bashReply("echo original"), 0)
	useAPI(t, api)
	chatOnce(t, ws, "go\n", "--yes")
	id, dbPath := sessionDB(t, home)

	var stored bool
	for _, e := range allEvents(t, dbPath) {
		if e.Call != nil && strings.Contains(string(e.Call.Input), marker) {
			stored = true
		}
	}
	require.True(t, stored, "the rewritten input must be in the log for this test to mean anything")

	r := startIn(t, home)
	w := newWatcher(r, id)
	w.followUntilDone(t, 1)
	r.stop(t)

	var rewritten bool
	for _, e := range w.events {
		raw, err := protojson.Marshal(e)
		require.NoError(t, err)
		require.NotContains(t, string(raw), marker)
		if e.GetRewritten() && e.GetCall() != nil {
			rewritten = true
			require.Empty(t, e.GetCall().GetInput())
		}
	}
	require.True(t, rewritten, "no rewritten call event was watched")
}
