package durable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// abandon simulates the process dying: stop renewing and drop the database
// handle without releasing the lease, so the next owner must wait it out.
func (h *Harness) abandon() {
	h.closeOnce.Do(func() {
		h.stopRenew()
		<-h.renewDone
		h.db.Close()
	})
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
	crashOn int32 // crash (cancel ctx) after the effect on this run number; 0 = never
	cancel  context.CancelFunc
}

func (t *fakeTool) Spec() ToolSpec { return ToolSpec{Name: t.name} }
func (t *fakeTool) Replay() Replay { return t.replay }
func (t *fakeTool) Execute(ctx context.Context, c ToolCall) (string, error) {
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
	h.Submit(ctx, "a", "first")
	h.Submit(ctx, "b", "second")
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
