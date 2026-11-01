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

func TestGateInvalidRewriteErrorsAndLeavesCallPending(t *testing.T) {
	ctx := context.Background()
	write := &fakeTool{name: "write_file"}
	h := openWith(t, ctx, Options{Model: gateModel(), Tools: []Tool{write}, LeaseTTL: ttl, Gate: rewriteGate(`{bad`)})
	defer h.Close()

	_, err := h.Prompt(ctx, "a", "go")
	require.Error(t, err)
	require.Equal(t, callPending, callStatus(t, h))
	require.EqualValues(t, 0, write.runs.Load())
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
