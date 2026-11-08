package durable

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// abandon simulates the process dying: stop renewing and drop the database
// handle without releasing the lease, so the next owner must wait it out.
func (h *Harness) abandon() {
	_ = h.Detach()
}

const ttl = 150 * time.Millisecond

// scriptedModel asks for write_file then read_file, then answers with a
// summary of the tool results it saw.
type scriptedModel struct {
	calls     atomic.Int32
	crashOnce func() // if set, called (once) during the first Generate
}

func (m *scriptedModel) Generate(ctx context.Context, history []Message, _ []ToolSpec) (Message, error) {
	m.calls.Add(1)
	if m.crashOnce != nil {
		f := m.crashOnce
		m.crashOnce = nil
		f()
		return Message{}, ctx.Err()
	}
	last := history[len(history)-1]
	if last.Role == RoleUser {
		return Message{Role: RoleAssistant, Text: "on it", ToolCalls: []ToolCall{
			{ID: "call_write", Name: "write_file", Input: json.RawMessage(`{"path":"a.txt"}`)},
			{ID: "call_read", Name: "read_file", Input: json.RawMessage(`{"path":"a.txt"}`)},
		}}, nil
	}
	var parts []string
	for _, msg := range history {
		if msg.Role == RoleTool {
			parts = append(parts, fmt.Sprintf("%s=%s", msg.ToolCallID, msg.Text))
		}
	}
	return Message{Role: RoleAssistant, Text: "done: " + strings.Join(parts, "; ")}, nil
}

type fakeTool struct {
	name    string
	replay  Replay
	runs    atomic.Int32
	input   atomic.Value // last input seen by Execute, as a string
	crashOn int32        // crash (cancel ctx) after the effect on this run number; 0 = never
	cancel  context.CancelFunc
}

func (t *fakeTool) Spec() ToolSpec { return ToolSpec{Name: t.name} }
func (t *fakeTool) Replay() Replay { return t.replay }
func (t *fakeTool) Execute(ctx context.Context, call ToolCall) (string, error) {
	t.input.Store(string(call.Input))
	n := t.runs.Add(1) // the side effect
	if n == t.crashOn {
		t.cancel() // the process dies after the effect, before the result is saved
		return "", ctx.Err()
	}
	return fmt.Sprintf("%s ok (run %d)", t.name, n), nil
}

func open(t *testing.T, ctx context.Context, path string, m Model, tools ...Tool) *Harness {
	t.Helper()
	h, err := Open(ctx, path, Options{Model: m, Tools: tools, LeaseTTL: ttl})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return h
}

func roles(msgs []Message) string {
	var r []string
	for _, m := range msgs {
		r = append(r, string(m.Role))
	}
	return strings.Join(r, ",")
}

func TestCrashDuringUnsafeToolIsNotReplayed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	model := &scriptedModel{}
	write := &fakeTool{name: "write_file", replay: ReplayNever, crashOn: 1}
	read := &fakeTool{name: "read_file", replay: ReplaySafe}

	crashCtx, cancel := context.WithCancel(context.Background())
	write.cancel = cancel
	h1 := open(t, crashCtx, path, model, write, read)
	if _, err := h1.Prompt(crashCtx, "req-1", "fix the bug"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected crash, got %v", err)
	}
	h1.abandon()

	// A new process opens the same file: it waits for the dead lease, then resumes.
	ctx := context.Background()
	h2 := open(t, ctx, path, model, write, read)
	defer h2.Close()
	if err := h2.Run(ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}

	sub, _ := h2.Get(ctx, "req-1")
	if sub.Status != StatusDone {
		t.Fatalf("status = %s (%s)", sub.Status, sub.Error)
	}
	if got := write.runs.Load(); got != 1 {
		t.Errorf("write_file ran %d times; an unsafe call must not be replayed", got)
	}
	if got := read.runs.Load(); got != 1 {
		t.Errorf("read_file ran %d times, want 1", got)
	}
	if got := model.calls.Load(); got != 2 {
		t.Errorf("model called %d times, want 2", got)
	}
	hist, _ := h2.History(ctx)
	if got := roles(hist); got != "user,assistant,tool,tool,assistant" {
		t.Errorf("transcript = %s", got)
	}
	if hist[2].Text != UnknownOutcome || !hist[2].IsError {
		t.Errorf("write result = %q, want the unknown-outcome error", hist[2].Text)
	}
	t.Logf("final answer: %s", sub.Result)

	// Exactly-once input: a retried request returns the first result and does no work.
	again, err := h2.Prompt(ctx, "req-1", "fix the bug")
	if err != nil || again.Result != sub.Result || model.calls.Load() != 2 {
		t.Errorf("duplicate request did new work: %+v err=%v calls=%d", again, err, model.calls.Load())
	}
}

func TestCrashDuringSafeToolIsReplayed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	model := &scriptedModel{}
	write := &fakeTool{name: "write_file", replay: ReplayNever}
	read := &fakeTool{name: "read_file", replay: ReplaySafe, crashOn: 1}

	crashCtx, cancel := context.WithCancel(context.Background())
	read.cancel = cancel
	h1 := open(t, crashCtx, path, model, write, read)
	_, _ = h1.Prompt(crashCtx, "req-1", "fix the bug")
	h1.abandon()

	ctx := context.Background()
	h2 := open(t, ctx, path, model, write, read)
	defer h2.Close()
	if err := h2.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := read.runs.Load(); got != 2 {
		t.Errorf("read_file ran %d times, want 2 (replayed)", got)
	}
	if got := write.runs.Load(); got != 1 {
		t.Errorf("write_file ran %d times, want 1", got)
	}
	hist, _ := h2.History(ctx)
	if hist[3].Text != "read_file ok (run 2)" {
		t.Errorf("read result = %q", hist[3].Text)
	}
}

func TestCrashDuringModelCallRetriesTheCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	crashCtx, cancel := context.WithCancel(context.Background())
	model := &scriptedModel{crashOnce: cancel}
	write := &fakeTool{name: "write_file"}
	read := &fakeTool{name: "read_file", replay: ReplaySafe}

	h1 := open(t, crashCtx, path, model, write, read)
	_, _ = h1.Prompt(crashCtx, "req-1", "fix the bug")
	h1.abandon()

	ctx := context.Background()
	h2 := open(t, ctx, path, model, write, read)
	defer h2.Close()
	if err := h2.Run(ctx); err != nil {
		t.Fatal(err)
	}
	hist, _ := h2.History(ctx)
	if got := roles(hist); got != "user,assistant,tool,tool,assistant" {
		t.Errorf("transcript = %s (the lost model call must leave no trace)", got)
	}
	if got := model.calls.Load(); got != 3 {
		t.Errorf("model called %d times, want 3 (1 lost + 2)", got)
	}
}

func TestLeaseKeepsSecondWriterOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	ctx := context.Background()
	h1 := open(t, ctx, path, &scriptedModel{})

	// While h1 is alive and renewing, nobody else gets in.
	short, cancel := context.WithTimeout(ctx, 3*ttl)
	defer cancel()
	if _, err := Open(short, path, Options{Model: &scriptedModel{}, LeaseTTL: ttl}); err == nil {
		t.Fatal("second process opened a live session")
	}

	// A clean Close hands over immediately, without waiting for the TTL.
	h1.Close()
	start := time.Now()
	h2 := open(t, ctx, path, &scriptedModel{})
	defer h2.Close()
	if d := time.Since(start); d > ttl {
		t.Errorf("handover after Close took %v", d)
	}
}

func TestDetachKeepsLeaseUntilTTL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	ctx := context.Background()
	h1 := open(t, ctx, path, &scriptedModel{})
	require.NoError(t, h1.Detach())
	require.NoError(t, h1.Close())

	start := time.Now()
	h2 := open(t, ctx, path, &scriptedModel{})
	defer h2.Close()
	require.Greater(t, time.Since(start), ttl/2)
}

func TestStaleOwnerCannotCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	ctx := context.Background()
	h1 := open(t, ctx, path, &scriptedModel{})
	// Freeze h1 (like a GC pause or a suspended laptop): no more renewals.
	h1.stopRenew()
	<-h1.renewDone

	h2 := open(t, ctx, path, &scriptedModel{}) // takes over after the TTL
	defer h2.Close()

	// h1 wakes up and tries to write. The fence rejects it.
	if _, err := h1.Submit(ctx, "req-stale", "hi"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale owner write: got %v, want ErrLeaseLost", err)
	}
	h1.db.Close()
}

func TestQueueRunsInOrderAcrossSubmissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	ctx := context.Background()
	model := &scriptedModel{}
	h := open(t, ctx, path, model, &fakeTool{name: "write_file"}, &fakeTool{name: "read_file", replay: ReplaySafe})
	defer h.Close()

	// Queue two inputs (e.g. a user typed while the agent was busy), then run.
	_, err := h.Submit(ctx, "a", "first")
	require.NoError(t, err)
	_, err = h.Submit(ctx, "b", "second")
	require.NoError(t, err)
	if err := h.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if s, _ := h.Get(ctx, id); s.Status != StatusDone {
			t.Errorf("%s: %s", id, s.Status)
		}
	}
	hist, _ := h.History(ctx)
	if hist[0].Text != "first" || hist[5].Text != "second" {
		t.Errorf("submissions ran out of order: %s", roles(hist))
	}
}

// blockingTool blocks until released, or until ctx is cancelled.
type blockingTool struct {
	name      string
	runs      atomic.Int32
	entered   chan struct{}
	release   chan struct{} // nil means wait for ctx instead
	cancelled chan struct{}
}

func newBlockingTool(name string) *blockingTool {
	return &blockingTool{name: name, entered: make(chan struct{}, 4), cancelled: make(chan struct{}, 4)}
}

func (t *blockingTool) Spec() ToolSpec { return ToolSpec{Name: t.name} }
func (t *blockingTool) Replay() Replay { return ReplayNever }
func (t *blockingTool) Execute(ctx context.Context, _ ToolCall) (string, error) {
	t.runs.Add(1)
	t.entered <- struct{}{}
	select {
	case <-ctx.Done():
		t.cancelled <- struct{}{}
		return "", ctx.Err()
	case <-t.release:
		return t.name + " ok", nil
	}
}

type modelFunc func(ctx context.Context, history []Message, tools []ToolSpec) (Message, error)

func (f modelFunc) Generate(ctx context.Context, history []Message, tools []ToolSpec) (Message, error) {
	return f(ctx, history, tools)
}

func lastUserText(history []Message) string {
	for _, m := range slices.Backward(history) {
		if m.Role == RoleUser {
			return m.Text
		}
	}
	return ""
}

func openWith(t *testing.T, ctx context.Context, opts Options) *Harness {
	t.Helper()
	h, err := Open(ctx, filepath.Join(t.TempDir(), "session.sqlite"), opts)
	require.NoError(t, err)
	return h
}

func TestLeaseLossCancelsRunningTool(t *testing.T) {
	ctx := context.Background()
	write := newBlockingTool("write_file")
	h := openWith(t, ctx, Options{
		Model: &scriptedModel{}, LeaseTTL: ttl,
		Tools: []Tool{write, &fakeTool{name: "read_file", replay: ReplaySafe}},
	})
	defer h.abandon()

	errc := make(chan error, 1)
	go func() {
		_, err := h.Prompt(ctx, "req-1", "fix the bug")
		errc <- err
	}()

	<-write.entered
	_, err := h.db.ExecContext(ctx, `UPDATE lease SET owner = 'thief' WHERE id = 1`)
	require.NoError(t, err)

	select {
	case err := <-errc:
		require.ErrorIs(t, err, ErrLeaseLost)
	case <-time.After(5 * time.Second):
		t.Fatal("Prompt did not return after the lease was lost")
	}
	select {
	case <-write.cancelled:
	default:
		t.Fatal("tool did not see cancellation")
	}
}

func TestLeaseLostSignalsOnlyOnLoss(t *testing.T) {
	ctx := context.Background()
	h := openWith(t, ctx, Options{Model: &scriptedModel{}, LeaseTTL: ttl})
	defer h.abandon()
	select {
	case <-h.LeaseLost():
		t.Fatal("LeaseLost closed on a healthy harness")
	default:
	}
	_, err := h.db.ExecContext(ctx, `UPDATE lease SET owner = 'thief' WHERE id = 1`)
	require.NoError(t, err)
	select {
	case <-h.LeaseLost():
	case <-time.After(5 * time.Second):
		t.Fatal("LeaseLost did not close after the lease was taken")
	}

	healthy := openWith(t, ctx, Options{Model: &scriptedModel{}, LeaseTTL: ttl})
	require.NoError(t, healthy.Close())
	select {
	case <-healthy.LeaseLost():
		t.Fatal("LeaseLost closed on Close")
	default:
	}
}

func TestConcurrentRunsDoNotDoubleExecute(t *testing.T) {
	ctx := context.Background()
	model := &scriptedModel{}
	write := newBlockingTool("write_file")
	write.release = make(chan struct{})
	read := &fakeTool{name: "read_file", replay: ReplaySafe}
	h := openWith(t, ctx, Options{Model: model, Tools: []Tool{write, read}, LeaseTTL: ttl})
	defer h.Close()

	_, err := h.Submit(ctx, "req-1", "fix the bug")
	require.NoError(t, err)

	errc := make(chan error, 2)
	go func() { errc <- h.Run(ctx) }()
	<-write.entered
	go func() { errc <- h.Run(ctx) }()
	time.Sleep(20 * time.Millisecond) // long enough for the second Run to race
	close(write.release)

	require.NoError(t, <-errc)
	require.NoError(t, <-errc)
	require.EqualValues(t, 1, write.runs.Load())
	require.EqualValues(t, 1, read.runs.Load())
	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.Equal(t, "user,assistant,tool,tool,assistant", roles(hist))
}

func TestModelFailureFailsSubmissionAndQueueContinues(t *testing.T) {
	ctx := context.Background()
	var badCalls atomic.Int32
	model := modelFunc(func(_ context.Context, history []Message, _ []ToolSpec) (Message, error) {
		if lastUserText(history) == "bad" {
			badCalls.Add(1)
			return Message{}, errors.New("provider exploded")
		}
		return Message{Role: RoleAssistant, Text: "fine"}, nil
	})
	h := openWith(t, ctx, Options{Model: model, LeaseTTL: ttl, ModelAttempts: 3, ModelBackoff: time.Millisecond})
	defer h.Close()

	_, err := h.Submit(ctx, "a", "bad")
	require.NoError(t, err)
	_, err = h.Submit(ctx, "b", "good")
	require.NoError(t, err)
	require.NoError(t, h.Run(ctx))

	a, err := h.Get(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, StatusFailed, a.Status)
	require.Contains(t, a.Error, "provider exploded")
	require.EqualValues(t, 1, badCalls.Load())

	b, err := h.Get(ctx, "b")
	require.NoError(t, err)
	require.Equal(t, StatusDone, b.Status)
}

func TestTransientModelErrorIsRetried(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	model := modelFunc(func(context.Context, []Message, []ToolSpec) (Message, error) {
		if calls.Add(1) <= 2 {
			return Message{}, Transient(errors.New("flaky"))
		}
		return Message{Role: RoleAssistant, Text: "fine"}, nil
	})
	h := openWith(t, ctx, Options{Model: model, LeaseTTL: ttl, ModelBackoff: time.Millisecond})
	defer h.Close()

	sub, err := h.Prompt(ctx, "a", "hi")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
	require.EqualValues(t, 3, calls.Load())
}

func TestCancelDuringBackoffLeavesSubmissionRunning(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	called := make(chan struct{}, 1)
	model := modelFunc(func(context.Context, []Message, []ToolSpec) (Message, error) {
		if calls.Add(1) == 1 {
			called <- struct{}{}
			return Message{}, Transient(errors.New("flaky"))
		}
		return Message{Role: RoleAssistant, Text: "fine"}, nil
	})
	h := openWith(t, ctx, Options{Model: model, LeaseTTL: ttl, ModelBackoff: time.Hour})
	defer h.Close()

	_, err := h.Submit(ctx, "a", "hi")
	require.NoError(t, err)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- h.Run(runCtx) }()
	<-called
	cancel()
	require.ErrorIs(t, <-errc, context.Canceled)

	sub, err := h.Get(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, StatusRunning, sub.Status)

	require.NoError(t, h.Run(ctx))
	sub, err = h.Get(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
}

// approvalModel asks for one tool call, then answers with the tool result.
func approvalModel(history []Message, _ []ToolSpec) (Message, error) {
	last := history[len(history)-1]
	if last.Role == RoleUser {
		return Message{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "c1", Name: "write_file", Input: json.RawMessage(`{}`)},
		}}, nil
	}
	return Message{Role: RoleAssistant, Text: "saw: " + last.Text}, nil
}

func callStatus(t *testing.T, h *Harness) string {
	t.Helper()
	var s string
	require.NoError(t, h.db.QueryRow(`SELECT status FROM tool_calls WHERE call_id = 'c1'`).Scan(&s))
	return s
}

func TestAskRunsBeforeStarted(t *testing.T) {
	ctx := context.Background()
	write := &fakeTool{name: "write_file"}
	var h *Harness
	var statusInAsk string
	var runsInAsk int32 = -1
	h = openWith(t, ctx, Options{
		Model: modelFunc(func(_ context.Context, hist []Message, s []ToolSpec) (Message, error) {
			return approvalModel(hist, s)
		}),
		Tools: []Tool{write}, LeaseTTL: ttl,
		Ask: func(_ context.Context, call ToolCall) (bool, error) {
			require.Equal(t, "c1", call.ID)
			statusInAsk = callStatus(t, h)
			runsInAsk = write.runs.Load()
			return true, nil
		},
	})
	defer h.Close()

	sub, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
	require.Equal(t, callPending, statusInAsk)
	require.EqualValues(t, 0, runsInAsk)
	require.EqualValues(t, 1, write.runs.Load())
}

func TestDeclinedCallDoesNotRunAndModelContinues(t *testing.T) {
	ctx := context.Background()
	write := &fakeTool{name: "write_file"}
	h := openWith(t, ctx, Options{
		Model: modelFunc(func(_ context.Context, hist []Message, s []ToolSpec) (Message, error) {
			return approvalModel(hist, s)
		}),
		Tools: []Tool{write}, LeaseTTL: ttl,
		Ask: func(context.Context, ToolCall) (bool, error) { return false, nil },
	})
	defer h.Close()

	sub, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
	require.EqualValues(t, 0, write.runs.Load())
	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.Equal(t, "user,assistant,tool,assistant", roles(hist))
	require.Equal(t, Declined, hist[2].Text)
	require.True(t, hist[2].IsError)
	require.Equal(t, "saw: "+Declined, sub.Result)
}

func TestCancelWhileWaitingForApprovalLeavesCallPending(t *testing.T) {
	ctx := context.Background()
	write := &fakeTool{name: "write_file"}
	var asked atomic.Int32
	waiting := make(chan struct{}, 1)
	h := openWith(t, ctx, Options{
		Model: modelFunc(func(_ context.Context, hist []Message, s []ToolSpec) (Message, error) {
			return approvalModel(hist, s)
		}),
		Tools: []Tool{write}, LeaseTTL: ttl,
		Ask: func(ctx context.Context, _ ToolCall) (bool, error) {
			if asked.Add(1) == 1 {
				waiting <- struct{}{}
				<-ctx.Done()
				return false, ctx.Err()
			}
			return true, nil
		},
	})
	defer h.Close()

	_, err := h.Submit(ctx, "a", "go")
	require.NoError(t, err)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- h.Run(runCtx) }()
	<-waiting
	cancel()
	require.ErrorIs(t, <-errc, context.Canceled)
	require.Equal(t, callPending, callStatus(t, h))
	require.EqualValues(t, 0, write.runs.Load())

	require.NoError(t, h.Run(ctx))
	require.EqualValues(t, 2, asked.Load())
	require.EqualValues(t, 1, write.runs.Load())
	sub, err := h.Get(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
}

func TestAskErrorIsReturnedAndCallStaysPending(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("prompt broke")
	h := openWith(t, ctx, Options{
		Model: modelFunc(func(_ context.Context, hist []Message, s []ToolSpec) (Message, error) {
			return approvalModel(hist, s)
		}),
		Tools: []Tool{&fakeTool{name: "write_file"}}, LeaseTTL: ttl,
		Ask: func(context.Context, ToolCall) (bool, error) { return false, boom },
	})
	defer h.Close()

	_, err := h.Prompt(ctx, "a", "go")
	require.ErrorIs(t, err, boom)
	require.Equal(t, callPending, callStatus(t, h))
}

func TestAskNotCalledForReplayedOrUnknownCalls(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	model := modelFunc(func(_ context.Context, hist []Message, s []ToolSpec) (Message, error) {
		return approvalModel(hist, s)
	})
	var asked atomic.Int32
	ask := func(context.Context, ToolCall) (bool, error) { asked.Add(1); return true, nil }

	// Crash after the effect: the call is left started.
	crashCtx, cancel := context.WithCancel(ctx)
	write := &fakeTool{name: "write_file", replay: ReplaySafe, crashOn: 1, cancel: cancel}
	h1, err := Open(crashCtx, path, Options{Model: model, Tools: []Tool{write}, LeaseTTL: ttl, Ask: ask})
	require.NoError(t, err)
	_, _ = h1.Prompt(crashCtx, "a", "go")
	require.EqualValues(t, 1, asked.Load())
	h1.abandon()

	h2, err := Open(ctx, path, Options{Model: model, Tools: []Tool{write}, LeaseTTL: ttl, Ask: ask})
	require.NoError(t, err)
	defer h2.Close()
	require.NoError(t, h2.Run(ctx))
	require.EqualValues(t, 1, asked.Load(), "replayed started call must not be re-approved")
	require.EqualValues(t, 2, write.runs.Load())

	// Unknown tool: never asked.
	h3 := openWith(t, ctx, Options{Model: model, LeaseTTL: ttl, Ask: ask})
	defer h3.Close()
	sub, err := h3.Prompt(ctx, "b", "go")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
	require.EqualValues(t, 1, asked.Load())
}

func TestStartedReplayNeverStillYieldsUnknownOutcomeWithAsk(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	model := modelFunc(func(_ context.Context, hist []Message, s []ToolSpec) (Message, error) {
		return approvalModel(hist, s)
	})
	var asked atomic.Int32
	ask := func(context.Context, ToolCall) (bool, error) { asked.Add(1); return true, nil }

	crashCtx, cancel := context.WithCancel(ctx)
	write := &fakeTool{name: "write_file", crashOn: 1, cancel: cancel}
	h1, err := Open(crashCtx, path, Options{Model: model, Tools: []Tool{write}, LeaseTTL: ttl, Ask: ask})
	require.NoError(t, err)
	_, _ = h1.Prompt(crashCtx, "a", "go")
	h1.abandon()

	h2, err := Open(ctx, path, Options{Model: model, Tools: []Tool{write}, LeaseTTL: ttl, Ask: ask})
	require.NoError(t, err)
	defer h2.Close()
	require.NoError(t, h2.Run(ctx))
	hist, err := h2.History(ctx)
	require.NoError(t, err)
	require.Equal(t, UnknownOutcome, hist[2].Text)
	require.EqualValues(t, 1, asked.Load())
	require.EqualValues(t, 1, write.runs.Load())
}

func TestTransientErrorExhaustionPausesRun(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	var down atomic.Bool
	down.Store(true)
	cause := errors.New("overloaded")
	model := modelFunc(func(context.Context, []Message, []ToolSpec) (Message, error) {
		calls.Add(1)
		if down.Load() {
			return Message{}, Transient(cause)
		}
		return Message{Role: RoleAssistant, Text: "fine"}, nil
	})
	h := openWith(t, ctx, Options{Model: model, LeaseTTL: ttl, ModelAttempts: 3, ModelBackoff: time.Millisecond})
	defer h.Close()

	_, err := h.Submit(ctx, "a", "hi")
	require.NoError(t, err)
	err = h.Run(ctx)
	require.ErrorIs(t, err, cause)
	require.EqualValues(t, 3, calls.Load())
	sub, err := h.Get(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, StatusRunning, sub.Status)

	down.Store(false)
	require.NoError(t, h.Run(ctx))
	sub, err = h.Get(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
}

func TestTransientHelper(t *testing.T) {
	require.NoError(t, Transient(nil))
	base := errors.New("x")
	err := fmt.Errorf("wrapped: %w", Transient(base))
	require.ErrorIs(t, err, base)
	require.True(t, isTransient(err))
	require.False(t, isTransient(base))
}

func TestMessageRawRoundTripsByteForByte(t *testing.T) {
	ctx := context.Background()
	raw := `[{"type":"thinking","thinking":"a<b && c>d","signature":"EqQB+/="},{"type":"text","text":"hi"}]`
	model := modelFunc(func(context.Context, []Message, []ToolSpec) (Message, error) {
		return Message{Role: RoleAssistant, Text: "hi", Raw: json.RawMessage(raw)}, nil
	})
	h := openWith(t, ctx, Options{Model: model, LeaseTTL: ttl})
	defer h.Close()

	_, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)
	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.Equal(t, raw, string(hist[1].Raw))
}

func TestOpenEscapesPath(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "we?ird #dir %41 x")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "s?#%41 .sqlite")

	h, err := Open(ctx, path, Options{Model: &scriptedModel{}, LeaseTTL: ttl})
	require.NoError(t, err)
	require.NoError(t, h.Close())

	_, err = os.Stat(path)
	require.NoError(t, err)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		require.True(t, strings.HasPrefix(e.Name(), "s?#%41 .sqlite"), e.Name())
	}
}

// assertToolResultsMatch checks every tool call in the history has exactly one tool message after it.
func assertToolResultsMatch(t *testing.T, hist []Message) {
	t.Helper()
	for i, m := range hist {
		for _, c := range m.ToolCalls {
			n := 0
			for _, later := range hist[i+1:] {
				if later.Role == RoleTool && later.ToolCallID == c.ID {
					n++
				}
			}
			require.Equal(t, 1, n, "tool call %q", c.ID)
		}
	}
}

// startRun runs h.Run in the background and returns a stop func that cancels it and returns its error.
func startRun(h *Harness) (stop func() error) {
	runCtx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- h.Run(runCtx) }()
	return func() error {
		cancel()
		return <-errc
	}
}

// threeCallModel asks for calls a, b and c in one message, then answers.
func threeCallModel(_ context.Context, hist []Message, _ []ToolSpec) (Message, error) {
	if hist[len(hist)-1].Role == RoleUser && lastUserText(hist) == "go" {
		return Message{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "ca", Name: "a", Input: json.RawMessage(`{}`)},
			{ID: "cb", Name: "b", Input: json.RawMessage(`{}`)},
			{ID: "cc", Name: "c", Input: json.RawMessage(`{}`)},
		}}, nil
	}
	return Message{Role: RoleAssistant, Text: "fine"}, nil
}

func TestCancelQueuedAppendsNothingAndRunSkipsIt(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	h := openWith(t, ctx, Options{LeaseTTL: ttl, Model: modelFunc(func(context.Context, []Message, []ToolSpec) (Message, error) {
		calls.Add(1)
		return Message{Role: RoleAssistant, Text: "fine"}, nil
	})})
	defer h.Close()

	_, err := h.Submit(ctx, "a", "hi")
	require.NoError(t, err)
	sub, err := h.Cancel(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, StatusCancelled, sub.Status)

	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.Empty(t, hist)
	require.NoError(t, h.Run(ctx))
	require.Zero(t, calls.Load())
	got, err := h.Get(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, StatusCancelled, got.Status)
}

func TestCancelRunningWithoutReplyAddsNoteAndNextPromptSeesIt(t *testing.T) {
	ctx := context.Background()
	entered := make(chan struct{}, 1)
	var seen []Message
	h := openWith(t, ctx, Options{LeaseTTL: ttl, Model: modelFunc(func(ctx context.Context, hist []Message, _ []ToolSpec) (Message, error) {
		if lastUserText(hist) == "first" {
			entered <- struct{}{}
			<-ctx.Done()
			return Message{}, ctx.Err()
		}
		seen = hist
		return Message{Role: RoleAssistant, Text: "fine"}, nil
	})})
	defer h.Close()

	_, err := h.Submit(ctx, "a", "first")
	require.NoError(t, err)
	stop := startRun(h)
	<-entered
	require.ErrorIs(t, stop(), context.Canceled)

	sub, err := h.Cancel(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, StatusCancelled, sub.Status)
	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.Equal(t, "user,user", roles(hist))
	require.Equal(t, RequestCancelled, hist[1].Text)

	next, err := h.Prompt(ctx, "b", "second")
	require.NoError(t, err)
	require.Equal(t, StatusDone, next.Status)
	require.Equal(t, "user,user,user", roles(seen))
	require.Equal(t, RequestCancelled, seen[1].Text)
}

func TestCancelRecordsOneResultPerUnfinishedCall(t *testing.T) {
	ctx := context.Background()
	a := &fakeTool{name: "a", replay: ReplaySafe}
	b := newBlockingTool("b")
	c := &fakeTool{name: "c"}
	h := openWith(t, ctx, Options{LeaseTTL: ttl, Model: modelFunc(threeCallModel), Tools: []Tool{a, b, c}})
	defer h.Close()

	_, err := h.Submit(ctx, "r1", "go")
	require.NoError(t, err)
	stop := startRun(h)
	<-b.entered
	require.ErrorIs(t, stop(), context.Canceled)

	sub, err := h.Cancel(ctx, "r1")
	require.NoError(t, err)
	require.Equal(t, StatusCancelled, sub.Status)

	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.Equal(t, "user,assistant,tool,tool,tool,user", roles(hist))
	require.Equal(t, "ca", hist[2].ToolCallID)
	require.Equal(t, "a ok (run 1)", hist[2].Text)
	require.False(t, hist[2].IsError)
	require.Equal(t, "cb", hist[3].ToolCallID)
	require.Equal(t, CancelledUnknown, hist[3].Text)
	require.True(t, hist[3].IsError)
	require.Equal(t, "cc", hist[4].ToolCallID)
	require.Equal(t, CallCancelled, hist[4].Text)
	require.True(t, hist[4].IsError)
	require.Equal(t, RequestCancelled, hist[5].Text)
	assertToolResultsMatch(t, hist)

	require.NoError(t, h.Run(ctx))
	require.EqualValues(t, 1, a.runs.Load())
	require.EqualValues(t, 1, b.runs.Load())
	require.Zero(t, c.runs.Load())
	after, err := h.History(ctx)
	require.NoError(t, err)
	require.Len(t, after, len(hist))
}

func TestCancelIsIdempotentForTerminalSubmissions(t *testing.T) {
	ctx := context.Background()
	h := openWith(t, ctx, Options{LeaseTTL: ttl, Model: modelFunc(func(context.Context, []Message, []ToolSpec) (Message, error) {
		return Message{Role: RoleAssistant, Text: "fine"}, nil
	})})
	defer h.Close()

	done, err := h.Prompt(ctx, "a", "hi")
	require.NoError(t, err)
	got, err := h.Cancel(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, done, got)
	require.Equal(t, StatusDone, got.Status)

	_, err = h.Submit(ctx, "b", "hi")
	require.NoError(t, err)
	_, err = h.Cancel(ctx, "b")
	require.NoError(t, err)
	again, err := h.Cancel(ctx, "b")
	require.NoError(t, err)
	require.Equal(t, StatusCancelled, again.Status)
	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.Equal(t, "user,assistant", roles(hist))
}

func TestCancelUnknownIDReturnsErrNoRows(t *testing.T) {
	ctx := context.Background()
	h := openWith(t, ctx, Options{LeaseTTL: ttl, Model: &scriptedModel{}})
	defer h.Close()

	_, err := h.Cancel(ctx, "missing")
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestCancelWaitsForRunToStop(t *testing.T) {
	ctx := context.Background()
	b := newBlockingTool("b")
	h := openWith(t, ctx, Options{LeaseTTL: ttl, Model: modelFunc(threeCallModel), Tools: []Tool{&fakeTool{name: "a"}, b, &fakeTool{name: "c"}}})
	defer h.Close()

	_, err := h.Submit(ctx, "r1", "go")
	require.NoError(t, err)
	stop := startRun(h)
	<-b.entered

	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = h.Cancel(short, "r1")
	require.ErrorIs(t, err, context.DeadlineExceeded)

	type result struct {
		sub Submission
		err error
	}
	res := make(chan result, 1)
	go func() {
		sub, err := h.Cancel(ctx, "r1")
		res <- result{sub, err}
	}()
	select {
	case <-res:
		t.Fatal("Cancel returned while Run was still working")
	case <-time.After(100 * time.Millisecond):
	}

	require.ErrorIs(t, stop(), context.Canceled)
	select {
	case r := <-res:
		require.NoError(t, r.err)
		require.Equal(t, StatusCancelled, r.sub.Status)
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel did not return after Run stopped")
	}
}

func TestCancelByStaleOwnerFailsWithLeaseLost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	ctx := context.Background()
	h1 := open(t, ctx, path, &scriptedModel{})
	_, err := h1.Submit(ctx, "a", "hi")
	require.NoError(t, err)
	h1.stopRenew()
	<-h1.renewDone

	h2 := open(t, ctx, path, &scriptedModel{})
	defer h2.Close()

	_, err = h1.Cancel(ctx, "a")
	require.ErrorIs(t, err, ErrLeaseLost)
	h1.db.Close()
	sub, err := h2.Get(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, StatusQueued, sub.Status)
}

func TestCancelledStaysCancelledAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	ctx := context.Background()
	model := &scriptedModel{}
	h1 := open(t, ctx, path, model)
	_, err := h1.Submit(ctx, "a", "hi")
	require.NoError(t, err)
	_, err = h1.Cancel(ctx, "a")
	require.NoError(t, err)
	require.NoError(t, h1.Close())

	h2 := open(t, ctx, path, model)
	defer h2.Close()
	sub, err := h2.Get(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, StatusCancelled, sub.Status)
	unfinished, err := h2.Unfinished(ctx)
	require.NoError(t, err)
	require.Empty(t, unfinished)
	require.NoError(t, h2.Run(ctx))
	require.Zero(t, model.calls.Load())
}

type readOnlyFake struct{ *fakeTool }

func (readOnlyFake) ReadOnly() bool { return true }

func gateModel() Model {
	return modelFunc(func(_ context.Context, hist []Message, s []ToolSpec) (Message, error) {
		return approvalModel(hist, s)
	})
}

func rewriteGate(input string) ToolGate {
	return func(context.Context, ToolCall) (Decision, error) {
		return Decision{Action: ActionAllow, Input: json.RawMessage(input)}, nil
	}
}

func storedInput(t *testing.T, h *Harness) string {
	t.Helper()
	var s string
	require.NoError(t, h.db.QueryRow(`SELECT input FROM tool_calls WHERE call_id = 'c1'`).Scan(&s))
	return s
}

func TestGateRewriteReachesExecute(t *testing.T) {
	ctx := context.Background()
	write := &fakeTool{name: "write_file"}
	h := openWith(t, ctx, Options{Model: gateModel(), Tools: []Tool{write}, LeaseTTL: ttl, Gate: rewriteGate(`{"x":1}`)})
	defer h.Close()

	sub, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
	require.Equal(t, `{"x":1}`, write.input.Load())
	require.Equal(t, `{"x":1}`, storedInput(t, h))
	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(hist[1].ToolCalls[0].Input), "the assistant message keeps the model's arguments")
}

func TestGateRewriteReplaysAfterCrash(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	opts := func(w Tool) Options {
		return Options{Model: gateModel(), Tools: []Tool{w}, LeaseTTL: ttl, Gate: rewriteGate(`{"x":1}`)}
	}

	crashCtx, cancel := context.WithCancel(ctx)
	write := &fakeTool{name: "write_file", replay: ReplaySafe, crashOn: 1, cancel: cancel}
	h1, err := Open(crashCtx, path, opts(write))
	require.NoError(t, err)
	_, _ = h1.Prompt(crashCtx, "a", "go")
	h1.abandon()

	// The second process has no rewriting gate, so only the stored input can carry the rewrite.
	o2 := opts(write)
	o2.Gate = func(context.Context, ToolCall) (Decision, error) { return Decision{}, errors.New("gate must not run") }
	h2, err := Open(ctx, path, o2)
	require.NoError(t, err)
	defer h2.Close()
	require.NoError(t, h2.Run(ctx))
	require.EqualValues(t, 2, write.runs.Load())
	require.Equal(t, `{"x":1}`, write.input.Load())
}

func TestGateRewriteReachesRecover(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	crashCtx, cancel := context.WithCancel(ctx)
	write := &recFake{fakeTool: &fakeTool{name: "write_file", crashOn: 1, cancel: cancel}}
	h1, err := Open(crashCtx, path, Options{Model: gateModel(), Tools: []Tool{write}, LeaseTTL: ttl, Gate: rewriteGate(`{"x":1}`)})
	require.NoError(t, err)
	_, _ = h1.Prompt(crashCtx, "a", "go")
	h1.abandon()

	var recovered string
	write.onRecover = func(_ context.Context, c ToolCall) { recovered = string(c.Input) }
	h2, err := Open(ctx, path, Options{Model: gateModel(), Tools: []Tool{write}, LeaseTTL: ttl})
	require.NoError(t, err)
	defer h2.Close()
	require.NoError(t, h2.Run(ctx))
	require.EqualValues(t, 1, write.n.Load())
	require.Equal(t, `{"x":1}`, recovered)
}

func TestAskSeesRewrittenInput(t *testing.T) {
	ctx := context.Background()
	var seen string
	h := openWith(t, ctx, Options{
		Model: gateModel(), Tools: []Tool{&fakeTool{name: "write_file"}}, LeaseTTL: ttl,
		Gate: func(context.Context, ToolCall) (Decision, error) {
			return Decision{Action: ActionAsk, Input: json.RawMessage(`{"x":1}`)}, nil
		},
		Ask: func(_ context.Context, call ToolCall) (bool, error) { seen = string(call.Input); return true, nil },
	})
	defer h.Close()

	_, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)
	require.Equal(t, `{"x":1}`, seen)
}

func TestGateDenyRecordsReasonOrDenied(t *testing.T) {
	for reason, want := range map[string]string{"no writes": "no writes", "": Denied} {
		ctx := context.Background()
		write := &fakeTool{name: "write_file"}
		h := openWith(t, ctx, Options{
			Model: gateModel(), Tools: []Tool{write}, LeaseTTL: ttl,
			Gate: func(context.Context, ToolCall) (Decision, error) {
				return Decision{Action: ActionDeny, Reason: reason, Input: json.RawMessage(`{"x":1}`)}, nil
			},
		})

		sub, err := h.Prompt(ctx, "a", "go")
		require.NoError(t, err)
		require.Equal(t, "saw: "+want, sub.Result)
		require.EqualValues(t, 0, write.runs.Load())
		require.JSONEq(t, `{}`, storedInput(t, h), "a rewrite on a denied call is not stored")
		hist, err := h.History(ctx)
		require.NoError(t, err)
		require.True(t, hist[2].IsError)
		h.Close()
	}
}

func TestGateFailsClosed(t *testing.T) {
	cases := map[string]ToolGate{
		"empty decision without Ask": func(context.Context, ToolCall) (Decision, error) { return Decision{}, nil },
		"unknown action": func(context.Context, ToolCall) (Decision, error) {
			return Decision{Action: Action(99)}, nil
		},
	}
	for name, gate := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			write := &fakeTool{name: "write_file"}
			asked := false
			opts := Options{Model: gateModel(), Tools: []Tool{write}, LeaseTTL: ttl, Gate: gate}
			if name == "unknown action" {
				opts.Ask = func(context.Context, ToolCall) (bool, error) { asked = true; return true, nil }
			}
			h := openWith(t, ctx, opts)
			defer h.Close()

			sub, err := h.Prompt(ctx, "a", "go")
			require.NoError(t, err)
			require.Equal(t, "saw: "+Denied, sub.Result)
			require.EqualValues(t, 0, write.runs.Load())
			require.False(t, asked)
		})
	}
}

func TestGateInvalidRewriteDeniesAndQueueContinues(t *testing.T) {
	ctx := context.Background()
	write := &fakeTool{name: "write_file"}
	h := openWith(t, ctx, Options{Model: gateModel(), Tools: []Tool{write}, LeaseTTL: ttl, Gate: rewriteGate(`{bad`)})
	defer h.Close()

	sub, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)
	require.Equal(t, "saw: "+InvalidRewrite, sub.Result)
	require.EqualValues(t, 0, write.runs.Load())
	require.JSONEq(t, `{}`, storedInput(t, h))

	sub, err = h.Prompt(ctx, "b", "next")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
}

func TestRewrittenResultCarriesNote(t *testing.T) {
	ctx := context.Background()
	h := openWith(t, ctx, Options{Model: gateModel(), Tools: []Tool{&fakeTool{name: "write_file"}}, LeaseTTL: ttl, Gate: rewriteGate(`{"x":1}`)})
	defer h.Close()

	_, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)
	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.Contains(t, hist[2].Text, Rewritten)
}

func TestJoinNote(t *testing.T) {
	require.Equal(t, "out", joinNote("out", ""))
	require.Equal(t, "note", joinNote("", "note"))
	require.Equal(t, "out note", joinNote("out", "note"))
}

func TestUnchangedRewriteCarriesNoNote(t *testing.T) {
	ctx := context.Background()
	h := openWith(t, ctx, Options{Model: gateModel(), Tools: []Tool{&fakeTool{name: "write_file"}}, LeaseTTL: ttl, Gate: rewriteGate(` { } `)})
	defer h.Close()

	_, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)
	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.NotContains(t, hist[2].Text, Rewritten)
}

func TestUnknownOutcomeCarriesRewriteNoteAfterCrash(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	crashCtx, cancel := context.WithCancel(ctx)
	write := &fakeTool{name: "write_file", crashOn: 1, cancel: cancel}
	h1, err := Open(crashCtx, path, Options{Model: gateModel(), Tools: []Tool{write}, LeaseTTL: ttl, Gate: rewriteGate(`{"x":1}`)})
	require.NoError(t, err)
	_, _ = h1.Prompt(crashCtx, "a", "go")
	h1.abandon()

	h2, err := Open(ctx, path, Options{Model: gateModel(), Tools: []Tool{write}, LeaseTTL: ttl})
	require.NoError(t, err)
	defer h2.Close()
	require.NoError(t, h2.Run(ctx))
	hist, err := h2.History(ctx)
	require.NoError(t, err)
	require.Equal(t, joinNote(UnknownOutcome, Rewritten), hist[2].Text)
}

func TestCancelCarriesRewriteNote(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	crashCtx, cancel := context.WithCancel(ctx)
	write := &fakeTool{name: "write_file", crashOn: 1, cancel: cancel}
	h1, err := Open(crashCtx, path, Options{Model: gateModel(), Tools: []Tool{write}, LeaseTTL: ttl, Gate: rewriteGate(`{"x":1}`)})
	require.NoError(t, err)
	_, _ = h1.Prompt(crashCtx, "a", "go")
	h1.abandon()

	h2, err := Open(ctx, path, Options{Model: gateModel(), Tools: []Tool{write}, LeaseTTL: ttl})
	require.NoError(t, err)
	defer h2.Close()
	_, err = h2.Cancel(ctx, "a")
	require.NoError(t, err)
	hist, err := h2.History(ctx)
	require.NoError(t, err)
	require.Equal(t, joinNote(CancelledUnknown, Rewritten), hist[2].Text)
}

func TestModelInputWithWhitespaceIsNotARewrite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	model := modelFunc(func(_ context.Context, hist []Message, _ []ToolSpec) (Message, error) {
		if hist[len(hist)-1].Role == RoleTool {
			return Message{Text: "done"}, nil
		}
		return Message{ToolCalls: []ToolCall{{ID: "c1", Name: "write_file", Input: json.RawMessage(`{ "a" : 1 }`)}}}, nil
	})
	crashCtx, cancel := context.WithCancel(ctx)
	write := &fakeTool{name: "write_file", crashOn: 1, cancel: cancel}
	h1, err := Open(crashCtx, path, Options{Model: model, Tools: []Tool{write}, LeaseTTL: ttl})
	require.NoError(t, err)
	_, _ = h1.Prompt(crashCtx, "a", "go")
	h1.abandon()

	h2, err := Open(ctx, path, Options{Model: model, Tools: []Tool{write}, LeaseTTL: ttl})
	require.NoError(t, err)
	defer h2.Close()
	require.NoError(t, h2.Run(ctx))
	hist, err := h2.History(ctx)
	require.NoError(t, err)
	require.Equal(t, UnknownOutcome, hist[2].Text)
}

func TestOpenRejectsDuplicateToolNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	_, err := Open(context.Background(), path, Options{
		Model: gateModel(), LeaseTTL: ttl,
		Tools: []Tool{readOnlyFake{&fakeTool{name: "write_file"}}, &fakeTool{name: "write_file"}},
	})
	require.ErrorContains(t, err, `duplicate tool "write_file"`)
}

func TestGateErrorLeavesCallPending(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("gate broke")
	h := openWith(t, ctx, Options{
		Model: gateModel(), Tools: []Tool{&fakeTool{name: "write_file"}}, LeaseTTL: ttl,
		Gate: func(context.Context, ToolCall) (Decision, error) { return Decision{}, boom },
	})
	defer h.Close()

	_, err := h.Prompt(ctx, "a", "go")
	require.ErrorIs(t, err, boom)
	require.Equal(t, callPending, callStatus(t, h))
}

func TestReadOnlyGate(t *testing.T) {
	gate := ReadOnlyGate([]Tool{
		readOnlyFake{&fakeTool{name: "read_file", replay: ReplaySafe}},
		&fakeTool{name: "fetch", replay: ReplaySafe},
	})
	for name, want := range map[string]Action{"read_file": ActionAllow, "fetch": ActionAsk, "other": ActionAsk} {
		d, err := gate(context.Background(), ToolCall{Name: name})
		require.NoError(t, err)
		require.Equal(t, want, d.Action, name)
	}
}

type collector struct {
	mu  sync.Mutex
	evs []Event
}

func (c *collector) observe(e Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evs = append(c.evs, e)
}

func (c *collector) all() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.evs)
}

func eventSummary(evs []Event) []string {
	var out []string
	for _, e := range evs {
		switch e.Kind {
		case EventSubmission:
			out = append(out, "submission:"+string(e.Status))
		case EventMessage:
			out = append(out, "message:"+string(e.Message.Role))
		default:
			out = append(out, string(e.Kind))
		}
	}
	return out
}

func requireIncreasing(t *testing.T, evs []Event) {
	t.Helper()
	for i := 1; i < len(evs); i++ {
		require.Greater(t, evs[i].Seq, evs[i-1].Seq)
	}
}

func observed(t *testing.T, gate ToolGate) (*Harness, *collector) {
	t.Helper()
	c := &collector{}
	h := openWith(t, context.Background(), Options{
		Model: gateModel(), Tools: []Tool{&fakeTool{name: "write_file"}}, LeaseTTL: ttl,
		Gate: gate, Observer: c.observe,
	})
	t.Cleanup(func() { _ = h.Close() })
	return h, c
}

func TestEventsFollowCommitOrder(t *testing.T) {
	ctx := context.Background()
	h, c := observed(t, nil)
	_, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)

	want := []string{
		"submission:queued", "message:user", "submission:running", "message:assistant",
		"call_started", "message:tool", "call_done", "message:assistant", "submission:done",
	}
	got := c.all()
	require.Equal(t, want, eventSummary(got))
	requireIncreasing(t, got)
	for _, e := range got {
		require.EqualValues(t, 1, e.Submission)
	}
	stored, err := h.Events(ctx, 0, 0)
	require.NoError(t, err)
	require.Equal(t, stored, got)
}

func TestCallStartedCarriesRewrittenInput(t *testing.T) {
	ctx := context.Background()
	h, c := observed(t, rewriteGate(`{"x":1}`))
	_, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)

	var started, assistant *Event
	for _, e := range c.all() {
		switch {
		case e.Kind == EventCallStarted:
			started = &e
		case e.Kind == EventMessage && e.Message.Role == RoleAssistant && assistant == nil:
			assistant = &e
		}
	}
	require.NotNil(t, started)
	require.True(t, started.Rewritten)
	require.Equal(t, "c1", started.Call.ID)
	require.Equal(t, "write_file", started.Call.Name)
	require.JSONEq(t, `{"x":1}`, string(started.Call.Input))
	require.JSONEq(t, `{}`, string(assistant.Message.ToolCalls[0].Input))
}

func TestUnchangedCallStartedIsNotRewritten(t *testing.T) {
	ctx := context.Background()
	h, c := observed(t, nil)
	_, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)
	for _, e := range c.all() {
		if e.Kind == EventCallStarted {
			require.False(t, e.Rewritten)
		}
	}
}

func TestEventsOmitRawButHistoryKeepsIt(t *testing.T) {
	ctx := context.Background()
	c := &collector{}
	model := modelFunc(func(context.Context, []Message, []ToolSpec) (Message, error) {
		return Message{Role: RoleAssistant, Text: "hi", Raw: json.RawMessage(`{"provider":"x"}`)}, nil
	})
	h := openWith(t, ctx, Options{Model: model, LeaseTTL: ttl, Observer: c.observe})
	defer h.Close()
	_, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)

	for _, e := range c.all() {
		if e.Message != nil {
			require.Empty(t, e.Message.Raw)
		}
	}
	stored, err := h.Events(ctx, 0, 0)
	require.NoError(t, err)
	for _, e := range stored {
		if e.Message != nil {
			require.Empty(t, e.Message.Raw)
		}
	}
	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.JSONEq(t, `{"provider":"x"}`, string(hist[1].Raw))
}

func TestEventsHonourAfterAndLimit(t *testing.T) {
	ctx := context.Background()
	h, _ := observed(t, nil)
	_, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)

	all, err := h.Events(ctx, 0, 0)
	require.NoError(t, err)
	require.Len(t, all, 9)

	page, err := h.Events(ctx, all[1].Seq, 3)
	require.NoError(t, err)
	require.Equal(t, all[2:5], page)

	none, err := h.Events(ctx, all[8].Seq, 10)
	require.NoError(t, err)
	require.Empty(t, none)
}

func TestEventsCatchUpAfterCrashWithoutDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	ctx := context.Background()
	c1 := &collector{}
	crashCtx, cancel := context.WithCancel(ctx)
	write := &fakeTool{name: "write_file", crashOn: 1, cancel: cancel}
	h1, err := Open(crashCtx, path, Options{Model: gateModel(), Tools: []Tool{write}, LeaseTTL: ttl, Observer: c1.observe})
	require.NoError(t, err)
	_, err = h1.Prompt(crashCtx, "a", "go")
	require.ErrorIs(t, err, context.Canceled)
	h1.abandon()
	seen := c1.all()
	require.NotEmpty(t, seen)
	last := seen[len(seen)-1].Seq

	c2 := &collector{}
	h2, err := Open(ctx, path, Options{Model: gateModel(), Tools: []Tool{write}, LeaseTTL: ttl, Observer: c2.observe})
	require.NoError(t, err)
	defer h2.Close()
	missed, err := h2.Events(ctx, last, 0)
	require.NoError(t, err)
	require.NoError(t, h2.Run(ctx))

	combined := slices.Concat(seen, missed, c2.all())
	requireIncreasing(t, combined)
	all, err := h2.Events(ctx, 0, 0)
	require.NoError(t, err)
	require.Equal(t, all, combined)
	require.Equal(t, "submission:done", eventSummary(all)[len(all)-1])
}

func TestObserverIsLiveOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	ctx := context.Background()
	h1 := open(t, ctx, path, &scriptedModel{})
	_, err := h1.Submit(ctx, "a", "hi")
	require.NoError(t, err)
	require.NoError(t, h1.Close())

	c := &collector{}
	h2, err := Open(ctx, path, Options{Model: &scriptedModel{}, LeaseTTL: ttl, Observer: c.observe})
	require.NoError(t, err)
	defer h2.Close()
	require.Empty(t, c.all())
	_, err = h2.Submit(ctx, "b", "again")
	require.NoError(t, err)
	got := c.all()
	require.Equal(t, []string{"submission:queued"}, eventSummary(got))
	require.EqualValues(t, 2, got[0].Submission)
}

func TestCancelDeliversCancelledAfterRunContextIsDone(t *testing.T) {
	ctx := context.Background()
	c := &collector{}
	entered := make(chan struct{}, 1)
	model := modelFunc(func(ctx context.Context, _ []Message, _ []ToolSpec) (Message, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return Message{}, ctx.Err()
	})
	h := openWith(t, ctx, Options{Model: model, LeaseTTL: ttl, Observer: c.observe})
	defer h.Close()

	_, err := h.Submit(ctx, "a", "go")
	require.NoError(t, err)
	stop := startRun(h)
	<-entered
	require.ErrorIs(t, stop(), context.Canceled)

	_, err = h.Cancel(ctx, "a")
	require.NoError(t, err)
	evs := c.all()
	require.Equal(t, []string{
		"submission:queued", "message:user", "submission:running", "message:user", "submission:cancelled",
	}, eventSummary(evs))
}

func TestFailedModelRecordsErrorOnEvent(t *testing.T) {
	ctx := context.Background()
	c := &collector{}
	model := modelFunc(func(context.Context, []Message, []ToolSpec) (Message, error) {
		return Message{}, errors.New("provider exploded")
	})
	h := openWith(t, ctx, Options{Model: model, LeaseTTL: ttl, Observer: c.observe})
	defer h.Close()
	_, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)

	evs := c.all()
	last := evs[len(evs)-1]
	require.Equal(t, EventSubmission, last.Kind)
	require.Equal(t, StatusFailed, last.Status)
	require.Contains(t, last.Error, "provider exploded")
}

func TestDuplicateRequestIDAddsNoEvent(t *testing.T) {
	ctx := context.Background()
	h, c := observed(t, nil)
	_, err := h.Submit(ctx, "a", "go")
	require.NoError(t, err)
	_, err = h.Submit(ctx, "a", "go")
	require.NoError(t, err)
	require.Len(t, c.all(), 1)
	stored, err := h.Events(ctx, 0, 0)
	require.NoError(t, err)
	require.Len(t, stored, 1)
}

func TestStaleOwnerAddsAndDeliversNoEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	ctx := context.Background()
	c := &collector{}
	h1, err := Open(ctx, path, Options{Model: &scriptedModel{}, LeaseTTL: ttl, Observer: c.observe})
	require.NoError(t, err)
	h1.stopRenew()
	<-h1.renewDone

	h2 := open(t, ctx, path, &scriptedModel{})
	defer h2.Close()

	_, err = h1.Submit(ctx, "stale", "hi")
	require.ErrorIs(t, err, ErrLeaseLost)
	h1.db.Close()
	require.Empty(t, c.all())
	evs, err := h2.Events(ctx, 0, 0)
	require.NoError(t, err)
	require.Empty(t, evs)
}

// preEventsSchema is the schema before the events table existed.
const preEventsSchema = `
CREATE TABLE lease (id INTEGER PRIMARY KEY CHECK (id = 1), owner TEXT NOT NULL, expires_at INTEGER NOT NULL);
CREATE TABLE submissions (seq INTEGER PRIMARY KEY AUTOINCREMENT, request_id TEXT NOT NULL UNIQUE, text TEXT NOT NULL,
	status TEXT NOT NULL, result TEXT, error TEXT, created_at INTEGER NOT NULL);
CREATE TABLE messages (seq INTEGER PRIMARY KEY AUTOINCREMENT, submission_seq INTEGER NOT NULL REFERENCES submissions(seq),
	role TEXT NOT NULL, body TEXT NOT NULL);
CREATE TABLE tool_calls (message_seq INTEGER NOT NULL REFERENCES messages(seq), position INTEGER NOT NULL,
	call_id TEXT NOT NULL, name TEXT NOT NULL, input TEXT NOT NULL, status TEXT NOT NULL, result TEXT,
	is_error INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (message_seq, position));
INSERT INTO submissions (request_id, text, status, created_at) VALUES ('old', 'hi', 'queued', 0);
`

func TestOldSessionWithoutEventsTableWorks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	ctx := context.Background()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = db.Exec(preEventsSchema)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	c := &collector{}
	h, err := Open(ctx, path, Options{Model: gateModel(), Tools: []Tool{&fakeTool{name: "write_file"}}, LeaseTTL: ttl, Observer: c.observe})
	require.NoError(t, err)
	defer h.Close()
	evs, err := h.Events(ctx, 0, 0)
	require.NoError(t, err)
	require.Empty(t, evs)

	require.NoError(t, h.Run(ctx))
	sub, err := h.Get(ctx, "old")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
	require.Equal(t, "submission:done", eventSummary(c.all())[len(c.all())-1])
}

func TestObserverMayCallHistoryAndEvents(t *testing.T) {
	ctx := context.Background()
	var h *Harness
	var calls atomic.Int32
	observer := func(e Event) {
		if _, err := h.History(ctx); err == nil {
			if _, err := h.Events(ctx, e.Seq-1, 1); err == nil {
				calls.Add(1)
			}
		}
	}
	var err error
	h, err = Open(ctx, filepath.Join(t.TempDir(), "session.sqlite"), Options{
		Model: gateModel(), Tools: []Tool{&fakeTool{name: "write_file"}}, LeaseTTL: ttl, Observer: observer,
	})
	require.NoError(t, err)
	defer h.Close()

	done := make(chan error, 1)
	go func() { _, err := h.Prompt(ctx, "a", "go"); done <- err }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: Observer calling History never returned")
	}
	require.EqualValues(t, 9, calls.Load())
}

func TestSubmitRacingRunDeliversInOrder(t *testing.T) {
	ctx := context.Background()
	h, c := observed(t, nil)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() {
			_, err := h.Submit(ctx, fmt.Sprintf("r%d", i), "go")
			errs <- err
		})
	}
	for range 3 {
		require.NoError(t, h.Run(ctx))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.NoError(t, h.Run(ctx))

	got := c.all()
	requireIncreasing(t, got)
	stored, err := h.Events(ctx, 0, 0)
	require.NoError(t, err)
	require.Equal(t, stored, got)
}

// A commit can return just as its context is cancelled, and the event must still be delivered.
func TestDeliverIgnoresCancelledContext(t *testing.T) {
	ctx := context.Background()
	var got []Event
	h := openWith(t, ctx, Options{Model: gateModel(), LeaseTTL: ttl, Observer: func(e Event) { got = append(got, e) }})
	defer h.Close()
	_, err := h.Submit(ctx, "a", "go")
	require.NoError(t, err)
	got = nil

	_, err = h.db.ExecContext(ctx, `INSERT INTO events (body) VALUES ('{"kind":"submission","submission":1,"status":"cancelled"}')`)
	require.NoError(t, err)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	h.deliver(cancelled)
	require.Len(t, got, 1)
	require.Equal(t, StatusCancelled, got[0].Status)
}

func TestNilInputRunsWithoutRewriteNote(t *testing.T) {
	ctx := context.Background()
	model := modelFunc(func(_ context.Context, hist []Message, _ []ToolSpec) (Message, error) {
		if hist[len(hist)-1].Role == RoleTool {
			return Message{Text: "done"}, nil
		}
		return Message{ToolCalls: []ToolCall{{ID: "c1", Name: "write_file"}}}, nil
	})
	write := &fakeTool{name: "write_file"}
	var got []Event
	h := openWith(t, ctx, Options{Model: model, Tools: []Tool{write}, LeaseTTL: ttl, Observer: func(e Event) { got = append(got, e) }})
	defer h.Close()

	sub, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
	require.EqualValues(t, 1, write.runs.Load())
	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.NotContains(t, hist[2].Text, Rewritten)
	for _, e := range got {
		require.False(t, e.Rewritten)
	}
}

func TestReplayedCallHasItsOwnEvent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	opts := func(w Tool) Options {
		return Options{Model: gateModel(), Tools: []Tool{w}, LeaseTTL: ttl, Gate: rewriteGate(`{"x":1}`)}
	}
	crashCtx, cancel := context.WithCancel(ctx)
	write := &fakeTool{name: "write_file", replay: ReplaySafe, crashOn: 1, cancel: cancel}
	h1, err := Open(crashCtx, path, opts(write))
	require.NoError(t, err)
	_, _ = h1.Prompt(crashCtx, "a", "go")
	h1.abandon()

	h2, err := Open(ctx, path, opts(write))
	require.NoError(t, err)
	defer h2.Close()
	require.NoError(t, h2.Run(ctx))
	require.EqualValues(t, 2, write.runs.Load())
	evs, err := h2.Events(ctx, 0, 0)
	require.NoError(t, err)
	var replayed []Event
	for _, e := range evs {
		if e.Kind == EventCallReplayed {
			replayed = append(replayed, e)
		}
	}
	require.Len(t, replayed, 1)
	require.True(t, replayed[0].Rewritten)
	require.JSONEq(t, `{"x":1}`, string(replayed[0].Call.Input))
}

func TestCallDoneCoversCallsThatNeverRan(t *testing.T) {
	ctx := context.Background()
	h, c := observed(t, func(context.Context, ToolCall) (Decision, error) {
		return Decision{Action: ActionDeny}, nil
	})
	_, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)

	got := c.all()
	require.Equal(t, []string{
		"submission:queued", "message:user", "submission:running", "message:assistant",
		"message:tool", "call_done", "message:assistant", "submission:done",
	}, eventSummary(got))
	done := got[5]
	require.True(t, done.IsError)
	require.Equal(t, "c1", done.Call.ID)
	require.Equal(t, "write_file", done.Call.Name)
	require.JSONEq(t, "null", string(done.Call.Input), "call_done must not copy input that may hold secrets")
}

func TestCallDoneReportsRewriteAndSuccess(t *testing.T) {
	ctx := context.Background()
	h, c := observed(t, rewriteGate(`{"x":1}`))
	_, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)
	for _, e := range c.all() {
		if e.Kind == EventCallDone {
			require.True(t, e.Rewritten)
			require.False(t, e.IsError)
			return
		}
	}
	require.Fail(t, "no call_done event")
}

func TestStartedCallWithVanishedToolRecordsUnknownOutcome(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	model := modelFunc(func(_ context.Context, hist []Message, s []ToolSpec) (Message, error) {
		return approvalModel(hist, s)
	})

	crashCtx, cancel := context.WithCancel(ctx)
	write := &fakeTool{name: "write_file", replay: ReplaySafe, crashOn: 1, cancel: cancel}
	h1, err := Open(crashCtx, path, Options{Model: model, Tools: []Tool{write}, LeaseTTL: ttl})
	require.NoError(t, err)
	_, _ = h1.Prompt(crashCtx, "a", "go")
	h1.abandon()

	h2, err := Open(ctx, path, Options{Model: model, LeaseTTL: ttl})
	require.NoError(t, err)
	defer h2.Close()
	require.NoError(t, h2.Run(ctx))
	hist, err := h2.History(ctx)
	require.NoError(t, err)
	require.Equal(t, UnknownOutcome, hist[2].Text)
	require.True(t, hist[2].IsError)
	require.EqualValues(t, 1, write.runs.Load())
}

func TestPendingCallWithUnknownToolRecordsUnknownTool(t *testing.T) {
	ctx := context.Background()
	h := openWith(t, ctx, Options{
		Model: modelFunc(func(_ context.Context, hist []Message, s []ToolSpec) (Message, error) {
			return approvalModel(hist, s)
		}),
		LeaseTTL: ttl,
	})
	defer h.Close()
	_, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)
	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.Equal(t, `unknown tool "write_file"`, hist[2].Text)
	require.True(t, hist[2].IsError)
}

func deferOpts(model Model, gate ToolGate, tools ...Tool) Options {
	return Options{Model: model, Tools: tools, LeaseTTL: ttl, Gate: gate, DeferApprovals: true}
}

func askGate(calls *atomic.Int32) ToolGate {
	return func(context.Context, ToolCall) (Decision, error) {
		calls.Add(1)
		return Decision{Action: ActionAsk}, nil
	}
}

func approvalStatuses(t *testing.T, h *Harness) []string {
	t.Helper()
	rows, err := h.db.Query(`SELECT status FROM approvals ORDER BY seq`)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}

func countKind(evs []Event, kind EventKind) int {
	n := 0
	for _, e := range evs {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// parkOne submits and runs until the first call is parked, returning its approval.
func parkOne(t *testing.T, ctx context.Context, h *Harness) Approval {
	t.Helper()
	_, err := h.Submit(ctx, "a", "go")
	require.NoError(t, err)
	require.ErrorIs(t, h.Run(ctx), ErrAwaitingApproval)
	a, ok, err := h.PendingApproval(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	return a
}

func TestDeferredApprovalSurvivesRestartAndRunsOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	write := &fakeTool{name: "write_file"}
	var gated atomic.Int32

	h1, err := Open(ctx, path, deferOpts(gateModel(), askGate(&gated), write))
	require.NoError(t, err)
	a := parkOne(t, ctx, h1)
	require.Equal(t, ApprovalPending, a.Status)
	require.Equal(t, "c1", a.Call.ID)
	require.Equal(t, "write_file", a.Call.Name)
	require.JSONEq(t, `{}`, string(a.Call.Input))
	require.False(t, a.Rewritten)
	sub, err := h1.Get(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, sub.Seq, a.Submission)
	require.Equal(t, StatusRunning, sub.Status)
	require.Equal(t, callPending, callStatus(t, h1))
	h1.abandon()

	h2, err := Open(ctx, path, deferOpts(gateModel(), askGate(&gated), write))
	require.NoError(t, err)
	defer h2.Close()
	again, ok, err := h2.PendingApproval(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, a, again)
	require.ErrorIs(t, h2.Run(ctx), ErrAwaitingApproval)
	require.Equal(t, []string{"pending"}, approvalStatuses(t, h2))

	decided, err := h2.Decide(ctx, a.Seq, true, "alice")
	require.NoError(t, err)
	require.Equal(t, ApprovalAllowed, decided.Status)
	require.Equal(t, "alice", decided.DecidedBy)
	require.NoError(t, h2.Run(ctx))

	require.EqualValues(t, 1, write.runs.Load())
	require.EqualValues(t, 1, gated.Load())
	sub, err = h2.Get(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
	_, ok, err = h2.PendingApproval(ctx)
	require.NoError(t, err)
	require.False(t, ok)

	evs, err := h2.Events(ctx, 0, 0)
	require.NoError(t, err)
	require.Equal(t, 1, countKind(evs, EventApprovalRequested))
	require.Equal(t, 1, countKind(evs, EventApprovalDecided))
	for _, e := range evs {
		if e.Kind == EventApprovalRequested {
			require.Equal(t, a.Seq, e.ApprovalSeq)
			require.Equal(t, "c1", e.Call.ID)
			require.JSONEq(t, `{}`, string(e.Call.Input))
		}
		if e.Kind == EventApprovalDecided {
			require.Equal(t, ApprovalAllowed, e.Approval)
			require.Equal(t, "alice", e.DecidedBy)
		}
	}
}

func TestCrashAfterDecideRunsOnceWithoutNewPrompt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	write := &fakeTool{name: "write_file"}
	var gated atomic.Int32

	h1, err := Open(ctx, path, deferOpts(gateModel(), askGate(&gated), write))
	require.NoError(t, err)
	a := parkOne(t, ctx, h1)
	_, err = h1.Decide(ctx, a.Seq, true, "alice")
	require.NoError(t, err)
	require.Equal(t, callPending, callStatus(t, h1))
	h1.abandon()

	h2, err := Open(ctx, path, deferOpts(gateModel(), askGate(&gated), write))
	require.NoError(t, err)
	defer h2.Close()
	require.NoError(t, h2.Run(ctx))
	require.EqualValues(t, 1, write.runs.Load())
	require.EqualValues(t, 1, gated.Load(), "the gate must not run again after a decision")
	evs, err := h2.Events(ctx, 0, 0)
	require.NoError(t, err)
	require.Equal(t, 1, countKind(evs, EventApprovalRequested))
}

func TestApprovedCallRunsStoredRewriteAndKeepsNote(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	write := &fakeTool{name: "write_file"}
	gate := func(context.Context, ToolCall) (Decision, error) {
		return Decision{Action: ActionAsk, Input: json.RawMessage(`{"x":1}`)}, nil
	}

	h1, err := Open(ctx, path, deferOpts(gateModel(), gate, write))
	require.NoError(t, err)
	a := parkOne(t, ctx, h1)
	require.JSONEq(t, `{"x":1}`, string(a.Call.Input))
	require.True(t, a.Rewritten)
	_, err = h1.Decide(ctx, a.Seq, true, "alice")
	require.NoError(t, err)
	h1.abandon()

	// The gate that would rewrite is gone, so only the stored input can carry the rewrite.
	h2, err := Open(ctx, path, deferOpts(gateModel(), func(context.Context, ToolCall) (Decision, error) {
		return Decision{}, errors.New("gate must not run")
	}, write))
	require.NoError(t, err)
	defer h2.Close()
	require.NoError(t, h2.Run(ctx))
	require.Equal(t, `{"x":1}`, write.input.Load())
	hist, err := h2.History(ctx)
	require.NoError(t, err)
	require.Contains(t, hist[2].Text, Rewritten)
	evs, err := h2.Events(ctx, 0, 0)
	require.NoError(t, err)
	for _, e := range evs {
		if e.Kind == EventApprovalRequested || e.Kind == EventCallStarted {
			require.True(t, e.Rewritten, e.Kind)
		}
	}
}

func TestDeclinedApprovalRecordsDeclined(t *testing.T) {
	ctx := context.Background()
	write := &fakeTool{name: "write_file"}
	h := openWith(t, ctx, deferOpts(gateModel(), nil, write))
	defer h.Close()
	a := parkOne(t, ctx, h)

	_, err := h.Decide(ctx, a.Seq, false, "bob")
	require.NoError(t, err)
	require.NoError(t, h.Run(ctx))
	require.Zero(t, write.runs.Load())
	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.Equal(t, "user,assistant,tool,assistant", roles(hist))
	require.Equal(t, Declined, hist[2].Text)
	require.True(t, hist[2].IsError)
	require.Equal(t, []string{"declined"}, approvalStatuses(t, h))
}

func TestCancelWhileParkedClosesApproval(t *testing.T) {
	ctx := context.Background()
	write := &fakeTool{name: "write_file"}
	c := &collector{}
	opts := deferOpts(gateModel(), nil, write)
	opts.Observer = c.observe
	h := openWith(t, ctx, opts)
	defer h.Close()
	a := parkOne(t, ctx, h)

	sub, err := h.Cancel(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, StatusCancelled, sub.Status)
	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.Equal(t, "user,assistant,tool,user", roles(hist))
	require.Equal(t, CallCancelled, hist[2].Text)
	assertToolResultsMatch(t, hist)
	require.Equal(t, []string{"cancelled"}, approvalStatuses(t, h))
	_, ok, err := h.PendingApproval(ctx)
	require.NoError(t, err)
	require.False(t, ok)

	got, err := h.Decide(ctx, a.Seq, true, "alice")
	require.ErrorIs(t, err, ErrApprovalDecided)
	require.Equal(t, ApprovalCancelled, got.Status)
	require.NoError(t, h.Run(ctx))
	require.Zero(t, write.runs.Load())

	var closing []Event
	for _, e := range c.all() {
		if e.Kind == EventApprovalDecided {
			closing = append(closing, e)
		}
	}
	require.Len(t, closing, 1)
	require.Equal(t, ApprovalCancelled, closing[0].Approval)
	require.Equal(t, a.Seq, closing[0].ApprovalSeq)
}

func TestUnknownToolWhileParkedClosesApproval(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	h1, err := Open(ctx, path, deferOpts(gateModel(), nil, &fakeTool{name: "write_file"}))
	require.NoError(t, err)
	parkOne(t, ctx, h1)
	h1.abandon()

	h2, err := Open(ctx, path, deferOpts(gateModel(), nil))
	require.NoError(t, err)
	defer h2.Close()
	require.NoError(t, h2.Run(ctx))
	hist, err := h2.History(ctx)
	require.NoError(t, err)
	require.Equal(t, "user,assistant,tool,assistant", roles(hist))
	require.Contains(t, hist[2].Text, "unknown tool")
	require.Equal(t, []string{"cancelled"}, approvalStatuses(t, h2))
	sub, err := h2.Get(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
}

func TestDecideRepeatedContradictoryAndUnknown(t *testing.T) {
	ctx := context.Background()
	c := &collector{}
	opts := deferOpts(gateModel(), nil, &fakeTool{name: "write_file"})
	opts.Observer = c.observe
	h := openWith(t, ctx, opts)
	defer h.Close()
	a := parkOne(t, ctx, h)

	first, err := h.Decide(ctx, a.Seq, true, "alice")
	require.NoError(t, err)
	again, err := h.Decide(ctx, a.Seq, true, "bob")
	require.NoError(t, err)
	require.Equal(t, first, again, "the same answer is a no-op that keeps the first decider")
	require.Equal(t, 1, countKind(c.all(), EventApprovalDecided))

	got, err := h.Decide(ctx, a.Seq, false, "bob")
	require.ErrorIs(t, err, ErrApprovalDecided)
	require.Equal(t, ApprovalAllowed, got.Status)
	require.Equal(t, "alice", got.DecidedBy)
	require.Equal(t, 1, countKind(c.all(), EventApprovalDecided))

	_, err = h.Decide(ctx, 999, true, "alice")
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestTwoCallsParkInOrder(t *testing.T) {
	ctx := context.Background()
	write := &fakeTool{name: "write_file"}
	model := modelFunc(func(_ context.Context, hist []Message, _ []ToolSpec) (Message, error) {
		if hist[len(hist)-1].Role == RoleUser {
			return Message{ToolCalls: []ToolCall{
				{ID: "c1", Name: "write_file", Input: json.RawMessage(`{"n":1}`)},
				{ID: "c2", Name: "write_file", Input: json.RawMessage(`{"n":2}`)},
			}}, nil
		}
		return Message{Text: "done"}, nil
	})
	h := openWith(t, ctx, deferOpts(model, nil, write))
	defer h.Close()

	first := parkOne(t, ctx, h)
	require.Equal(t, "c1", first.Call.ID)
	_, err := h.Decide(ctx, first.Seq, true, "alice")
	require.NoError(t, err)
	require.ErrorIs(t, h.Run(ctx), ErrAwaitingApproval)
	require.EqualValues(t, 1, write.runs.Load())

	second, ok, err := h.PendingApproval(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "c2", second.Call.ID)
	require.Greater(t, second.Seq, first.Seq)
	_, err = h.Decide(ctx, second.Seq, true, "alice")
	require.NoError(t, err)
	require.NoError(t, h.Run(ctx))
	require.EqualValues(t, 2, write.runs.Load())
	require.Equal(t, `{"n":2}`, write.input.Load())
}

func TestParkedSubmissionBlocksTheQueue(t *testing.T) {
	ctx := context.Background()
	write := &fakeTool{name: "write_file"}
	h := openWith(t, ctx, deferOpts(gateModel(), nil, write))
	defer h.Close()
	_, err := h.Submit(ctx, "a", "go")
	require.NoError(t, err)
	_, err = h.Submit(ctx, "b", "go")
	require.NoError(t, err)

	decisions := 0
	for {
		err := h.Run(ctx)
		if !errors.Is(err, ErrAwaitingApproval) {
			require.NoError(t, err)
			break
		}
		b, err := h.Get(ctx, "b")
		require.NoError(t, err)
		if decisions == 0 {
			require.Equal(t, StatusQueued, b.Status, "b must wait behind the parked a")
		}
		a, ok, err := h.PendingApproval(ctx)
		require.NoError(t, err)
		require.True(t, ok)
		_, err = h.Decide(ctx, a.Seq, true, "alice")
		require.NoError(t, err)
		decisions++
	}
	require.Equal(t, 2, decisions)
	require.EqualValues(t, 2, write.runs.Load())
	for _, id := range []string{"a", "b"} {
		sub, err := h.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, StatusDone, sub.Status)
	}
}

func TestPendingApprovalReopenedWithAsk(t *testing.T) {
	for name, tc := range map[string]struct {
		answer     bool
		wantStatus string
		wantRuns   int32
	}{
		"allow":   {true, "allowed", 1},
		"decline": {false, "declined", 0},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "session.sqlite")
			write := &fakeTool{name: "write_file"}
			h1, err := Open(ctx, path, deferOpts(gateModel(), nil, write))
			require.NoError(t, err)
			parkOne(t, ctx, h1)
			h1.abandon()

			var seen string
			c := &collector{}
			h2, err := Open(ctx, path, Options{Model: gateModel(), Tools: []Tool{write}, LeaseTTL: ttl, Observer: c.observe,
				Ask: func(_ context.Context, call ToolCall) (bool, error) { seen = string(call.Input); return tc.answer, nil }})
			require.NoError(t, err)
			defer h2.Close()
			require.NoError(t, h2.Run(ctx))
			require.JSONEq(t, `{}`, seen)
			require.Equal(t, tc.wantRuns, write.runs.Load())
			require.Equal(t, []string{tc.wantStatus}, approvalStatuses(t, h2))
			var by string
			require.NoError(t, h2.db.QueryRow(`SELECT decided_by FROM approvals`).Scan(&by))
			require.Equal(t, "ask", by)
			require.Equal(t, 1, countKind(c.all(), EventApprovalDecided))
		})
	}
}

func TestDecideWinsOverAskOnAReopenedApproval(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	write := &fakeTool{name: "write_file"}
	h1, err := Open(ctx, path, deferOpts(gateModel(), nil, write))
	require.NoError(t, err)
	a := parkOne(t, ctx, h1)
	h1.abandon()

	var h2 *Harness
	h2, err = Open(ctx, path, Options{Model: gateModel(), Tools: []Tool{write}, LeaseTTL: ttl,
		Ask: func(ctx context.Context, _ ToolCall) (bool, error) {
			_, err := h2.Decide(ctx, a.Seq, false, "server")
			return true, err
		}})
	require.NoError(t, err)
	defer h2.Close()
	require.NoError(t, h2.Run(ctx))
	require.Zero(t, write.runs.Load())
	require.Equal(t, []string{"declined"}, approvalStatuses(t, h2))
	var by string
	require.NoError(t, h2.db.QueryRow(`SELECT decided_by FROM approvals`).Scan(&by))
	require.Equal(t, "server", by)
}

func TestPendingApprovalAskErrorLeavesItPending(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	write := &fakeTool{name: "write_file"}
	h1, err := Open(ctx, path, deferOpts(gateModel(), nil, write))
	require.NoError(t, err)
	parkOne(t, ctx, h1)
	h1.abandon()

	boom := errors.New("prompt broke")
	h2, err := Open(ctx, path, Options{Model: gateModel(), Tools: []Tool{write}, LeaseTTL: ttl,
		Ask: func(context.Context, ToolCall) (bool, error) { return false, boom }})
	require.NoError(t, err)
	defer h2.Close()
	require.ErrorIs(t, h2.Run(ctx), boom)
	require.Equal(t, []string{"pending"}, approvalStatuses(t, h2))
	require.Equal(t, callPending, callStatus(t, h2))
	require.Zero(t, write.runs.Load())
}

func TestPendingApprovalWithNeitherDeferNorAskIsDenied(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	write := &fakeTool{name: "write_file"}
	h1, err := Open(ctx, path, deferOpts(gateModel(), nil, write))
	require.NoError(t, err)
	parkOne(t, ctx, h1)
	h1.abandon()

	h2, err := Open(ctx, path, Options{Model: gateModel(), Tools: []Tool{write}, LeaseTTL: ttl})
	require.NoError(t, err)
	defer h2.Close()
	require.NoError(t, h2.Run(ctx))
	require.Zero(t, write.runs.Load(), "a parked call must never auto-allow")
	hist, err := h2.History(ctx)
	require.NoError(t, err)
	require.Equal(t, Denied, hist[2].Text)
	require.Equal(t, []string{"cancelled"}, approvalStatuses(t, h2))
}

func TestDeferApprovalsWinsOverAsk(t *testing.T) {
	ctx := context.Background()
	var asked atomic.Int32
	opts := deferOpts(gateModel(), nil, &fakeTool{name: "write_file"})
	opts.Ask = func(context.Context, ToolCall) (bool, error) { asked.Add(1); return true, nil }
	h := openWith(t, ctx, opts)
	defer h.Close()
	parkOne(t, ctx, h)
	require.ErrorIs(t, h.Run(ctx), ErrAwaitingApproval)
	require.Zero(t, asked.Load())
}

func TestAwaitingApprovalErrorNamesTheCall(t *testing.T) {
	ctx := context.Background()
	h := openWith(t, ctx, deferOpts(gateModel(), nil, &fakeTool{name: "write_file"}))
	defer h.Close()
	_, err := h.Submit(ctx, "a", "go")
	require.NoError(t, err)

	first := h.Run(ctx)
	a, ok, err := h.PendingApproval(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	for _, got := range []error{first, h.Run(ctx)} {
		require.ErrorIs(t, got, ErrAwaitingApproval)
		require.ErrorContains(t, got, `tool "write_file" call "`+a.Call.ID+`"`)
	}
}

func TestDeferApprovalsStillHonoursGateAllowAndDeny(t *testing.T) {
	ctx := context.Background()
	write := &fakeTool{name: "write_file"}
	h := openWith(t, ctx, deferOpts(gateModel(), func(context.Context, ToolCall) (Decision, error) {
		return Decision{Action: ActionAllow}, nil
	}, write))
	defer h.Close()
	sub, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
	require.EqualValues(t, 1, write.runs.Load())
	require.Empty(t, approvalStatuses(t, h))

	d := openWith(t, ctx, deferOpts(gateModel(), func(context.Context, ToolCall) (Decision, error) {
		return Decision{Action: ActionDeny}, nil
	}, write))
	defer d.Close()
	sub, err = d.Prompt(ctx, "a", "go")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
	require.EqualValues(t, 1, write.runs.Load())
	require.Empty(t, approvalStatuses(t, d))
}

func TestInvalidRewriteIsNeverParked(t *testing.T) {
	ctx := context.Background()
	write := &fakeTool{name: "write_file"}
	h := openWith(t, ctx, deferOpts(gateModel(), func(context.Context, ToolCall) (Decision, error) {
		return Decision{Action: ActionAsk, Input: json.RawMessage(`{bad`)}, nil
	}, write))
	defer h.Close()
	sub, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
	require.Zero(t, write.runs.Load())
	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.Equal(t, InvalidRewrite, hist[2].Text)
	require.Empty(t, approvalStatuses(t, h))
}

func TestDecideByStaleOwnerFailsWithLeaseLost(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	h1, err := Open(ctx, path, deferOpts(gateModel(), nil, &fakeTool{name: "write_file"}))
	require.NoError(t, err)
	a := parkOne(t, ctx, h1)
	h1.stopRenew()
	<-h1.renewDone

	h2 := open(t, ctx, path, gateModel(), &fakeTool{name: "write_file"})
	defer h2.Close()

	_, err = h1.Decide(ctx, a.Seq, true, "alice")
	require.ErrorIs(t, err, ErrLeaseLost)
	h1.db.Close()
	require.Equal(t, []string{"pending"}, approvalStatuses(t, h2))
}

func TestDeferredApprovalWithNilInput(t *testing.T) {
	ctx := context.Background()
	write := &fakeTool{name: "write_file"}
	model := modelFunc(func(_ context.Context, hist []Message, _ []ToolSpec) (Message, error) {
		if hist[len(hist)-1].Role == RoleTool {
			return Message{Text: "done"}, nil
		}
		return Message{ToolCalls: []ToolCall{{ID: "c1", Name: "write_file"}}}, nil
	})
	c := &collector{}
	opts := deferOpts(model, nil, write)
	opts.Observer = c.observe
	h := openWith(t, ctx, opts)
	defer h.Close()

	a := parkOne(t, ctx, h)
	require.Empty(t, a.Call.Input)
	require.False(t, a.Rewritten)
	_, err := h.Decide(ctx, a.Seq, true, "alice")
	require.NoError(t, err)
	require.NoError(t, h.Run(ctx))
	require.EqualValues(t, 1, write.runs.Load())
	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.NotContains(t, hist[2].Text, Rewritten)
	for _, e := range c.all() {
		require.False(t, e.Rewritten)
	}
}

func TestReadStatusBesideTheLeaseHolder(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	var gated atomic.Int32
	h, err := Open(ctx, path, deferOpts(gateModel(), askGate(&gated), &fakeTool{name: "write_file"}))
	require.NoError(t, err)
	defer func() { _ = h.Close() }()
	parkOne(t, ctx, h)
	_, err = h.Submit(ctx, "b", "later")
	require.NoError(t, err)

	st, err := ReadStatus(ctx, path)
	require.NoError(t, err)
	require.Equal(t, Status{PendingApprovals: 1, Unfinished: 2}, st)
}

func TestReadStatusEmptySession(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.sqlite")
	h, err := Open(ctx, path, Options{Model: gateModel()})
	require.NoError(t, err)
	require.NoError(t, h.Close())

	st, err := ReadStatus(ctx, path)
	require.NoError(t, err)
	require.Equal(t, Status{}, st)
}

func TestReadStatusMissingDatabase(t *testing.T) {
	_, err := ReadStatus(context.Background(), filepath.Join(t.TempDir(), "none.sqlite"))
	require.Error(t, err)
}
