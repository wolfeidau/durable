package durable

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type deltaLog struct {
	mu sync.Mutex
	ds []Delta
}

func (l *deltaLog) add(d Delta) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ds = append(l.ds, d)
}

func (l *deltaLog) all() []Delta {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Delta(nil), l.ds...)
}

func joinText(ds []Delta, kind DeltaKind) string {
	var b strings.Builder
	for _, d := range ds {
		if d.Kind == kind {
			b.WriteString(d.Text)
		}
	}
	return b.String()
}

// emitTool emits chunks as tool output, optionally also under WithoutToolOutput.
type emitTool struct {
	name    string
	replay  Replay
	chunks  []string
	runs    atomic.Int32
	crashOn int32
	cancel  context.CancelFunc
	silent  bool // also emit under WithoutToolOutput, which must be dropped
}

func (t *emitTool) Spec() ToolSpec { return ToolSpec{Name: t.name} }
func (t *emitTool) Replay() Replay { return t.replay }
func (t *emitTool) Execute(ctx context.Context, _ ToolCall) (string, error) {
	n := t.runs.Add(1)
	for _, c := range t.chunks {
		EmitToolOutput(ctx, c)
		if t.silent {
			EmitToolOutput(WithoutToolOutput(ctx), "hidden")
		}
	}
	if n == t.crashOn {
		t.cancel()
		return "", ctx.Err()
	}
	return strings.Join(t.chunks, ""), nil
}

func callOnce() func(context.Context, []Message, []ToolSpec) (Message, error) {
	return func(_ context.Context, hist []Message, _ []ToolSpec) (Message, error) {
		if len(hist) > 0 && hist[len(hist)-1].Role == RoleUser {
			return Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "t", Input: json.RawMessage(`{}`)}}}, nil
		}
		return Message{Role: RoleAssistant, Text: "done"}, nil
	}
}

func TestTextDeltasArriveInOrderAndMatchTheCommittedMessage(t *testing.T) {
	ctx := context.Background()
	var log deltaLog
	model := modelFunc(func(ctx context.Context, _ []Message, _ []ToolSpec) (Message, error) {
		for _, s := range []string{"he", "", "llo"} {
			EmitText(ctx, s)
		}
		return Message{Role: RoleAssistant, Text: "hello"}, nil
	})
	h := openWith(t, ctx, Options{Model: model, LeaseTTL: ttl, OnDelta: log.add})
	defer h.Close()

	sub, err := h.Prompt(ctx, "a", "hi")
	require.NoError(t, err)
	require.Equal(t, []Delta{
		{Kind: DeltaText, Submission: sub.Seq, Text: "he"},
		{Kind: DeltaText, Submission: sub.Seq, Text: "llo"},
	}, log.all())
	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.Equal(t, hist[len(hist)-1].Text, joinText(log.all(), DeltaText))
}

func TestDeltaResetFiresOncePerRetryNotOnTheFirstAttempt(t *testing.T) {
	ctx := context.Background()
	var log deltaLog
	var calls atomic.Int32
	model := modelFunc(func(ctx context.Context, _ []Message, _ []ToolSpec) (Message, error) {
		EmitText(ctx, "partial")
		if calls.Add(1) <= 2 {
			return Message{}, Transient(errors.New("flaky"))
		}
		return Message{Role: RoleAssistant, Text: "partial"}, nil
	})
	h := openWith(t, ctx, Options{Model: model, LeaseTTL: ttl, ModelBackoff: time.Millisecond, OnDelta: log.add})
	defer h.Close()

	_, err := h.Prompt(ctx, "a", "hi")
	require.NoError(t, err)
	var kinds []DeltaKind
	for _, d := range log.all() {
		kinds = append(kinds, d.Kind)
	}
	require.Equal(t, []DeltaKind{DeltaText, DeltaReset, DeltaText, DeltaReset, DeltaText}, kinds)
}

func TestDeltaResetCountsAttemptsPerModelCall(t *testing.T) {
	ctx := context.Background()
	var log deltaLog
	var calls atomic.Int32
	model := modelFunc(func(ctx context.Context, hist []Message, _ []ToolSpec) (Message, error) {
		if calls.Add(1) == 2 {
			return Message{}, Transient(errors.New("flaky"))
		}
		return callOnce()(ctx, hist, nil)
	})
	h := openWith(t, ctx, Options{Model: model, Tools: []Tool{&emitTool{name: "t"}}, LeaseTTL: ttl,
		ModelBackoff: time.Millisecond, OnDelta: log.add})
	defer h.Close()

	_, err := h.Prompt(ctx, "a", "hi")
	require.NoError(t, err)
	// Call 1 succeeds, call 2 fails once then succeeds: one reset in total.
	require.Len(t, log.all(), 1)
	require.Equal(t, DeltaReset, log.all()[0].Kind)
}

func TestCompactorEmitsNoDeltas(t *testing.T) {
	ctx := context.Background()
	var log deltaLog
	var compacts atomic.Int32
	comp := compactorFunc(func(ctx context.Context, _ CompactRequest) (Compaction, bool, error) {
		compacts.Add(1)
		EmitText(ctx, "summary")
		EmitToolOutput(ctx, "summary")
		return Compaction{}, false, nil
	})
	model := modelFunc(func(ctx context.Context, _ []Message, _ []ToolSpec) (Message, error) {
		EmitText(ctx, "reply")
		return Message{Role: RoleAssistant, Text: "reply"}, nil
	})
	h := openWith(t, ctx, Options{Model: model, Compactor: comp, LeaseTTL: ttl, OnDelta: log.add})
	defer h.Close()

	_, err := h.Prompt(ctx, "a", "hi")
	require.NoError(t, err)
	require.Positive(t, compacts.Load())
	require.Equal(t, "reply", joinText(log.all(), DeltaText))
	require.Len(t, log.all(), 1)
}

func TestToolDeltasCarryCallIDAndSubmission(t *testing.T) {
	ctx := context.Background()
	var log deltaLog
	tool := &emitTool{name: "t", chunks: []string{"a", "b"}, silent: true}
	h := openWith(t, ctx, Options{Model: modelFunc(func(ctx context.Context, hist []Message, _ []ToolSpec) (Message, error) {
		return callOnce()(ctx, hist, nil)
	}), Tools: []Tool{tool}, LeaseTTL: ttl, OnDelta: log.add})
	defer h.Close()

	sub, err := h.Prompt(ctx, "a", "hi")
	require.NoError(t, err)
	require.Equal(t, []Delta{
		{Kind: DeltaToolOutput, Submission: sub.Seq, CallID: "c1", Text: "a"},
		{Kind: DeltaToolOutput, Submission: sub.Seq, CallID: "c1", Text: "b"},
	}, log.all())
}

func TestToolDeltasOnReplaySafeReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	var log deltaLog
	crashCtx, cancel := context.WithCancel(context.Background())
	tool := &emitTool{name: "t", replay: ReplaySafe, chunks: []string{"x"}, crashOn: 1, cancel: cancel}
	model := modelFunc(func(ctx context.Context, hist []Message, _ []ToolSpec) (Message, error) {
		return callOnce()(ctx, hist, nil)
	})
	opts := Options{Model: model, Tools: []Tool{tool}, LeaseTTL: ttl, OnDelta: log.add}

	h1, err := Open(crashCtx, path, opts)
	require.NoError(t, err)
	_, _ = h1.Prompt(crashCtx, "a", "hi")
	h1.abandon()

	ctx := context.Background()
	h2, err := Open(ctx, path, opts)
	require.NoError(t, err)
	defer h2.Close()
	require.NoError(t, h2.Run(ctx))

	require.EqualValues(t, 2, tool.runs.Load())
	want := Delta{Kind: DeltaToolOutput, Submission: 1, CallID: "c1", Text: "x"}
	require.Equal(t, []Delta{want, want}, log.all())
}

func TestDeltasAddNoEvents(t *testing.T) {
	run := func(onDelta func(Delta)) []string {
		ctx := context.Background()
		c := &collector{}
		tool := &emitTool{name: "t", chunks: []string{"a"}}
		model := modelFunc(func(ctx context.Context, hist []Message, _ []ToolSpec) (Message, error) {
			EmitText(ctx, "x")
			return callOnce()(ctx, hist, nil)
		})
		h := openWith(t, ctx, Options{Model: model, Tools: []Tool{tool}, LeaseTTL: ttl, Observer: c.observe, OnDelta: onDelta})
		defer h.Close()
		_, err := h.Prompt(ctx, "a", "hi")
		require.NoError(t, err)
		evs, err := h.Events(ctx, 0, 0)
		require.NoError(t, err)
		require.Equal(t, c.all(), evs)
		return eventSummary(evs)
	}
	var log deltaLog
	require.Equal(t, run(nil), run(log.add))
	require.NotEmpty(t, log.all())
}

func TestNilOnDeltaAndNoSinkAreSafe(t *testing.T) {
	ctx := context.Background()
	require.NotPanics(t, func() {
		EmitText(ctx, "x")
		EmitToolOutput(ctx, "x")
		require.Equal(t, ctx, WithoutToolOutput(ctx))
	})

	// The model and tool emit, and nothing is installed to receive it.
	model := modelFunc(func(ctx context.Context, hist []Message, _ []ToolSpec) (Message, error) {
		EmitText(ctx, "x")
		return callOnce()(ctx, hist, nil)
	})
	h := openWith(t, ctx, Options{Model: model, Tools: []Tool{&emitTool{name: "t", chunks: []string{"a"}}}, LeaseTTL: ttl})
	defer h.Close()
	sub, err := h.Prompt(ctx, "a", "hi")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
}

func TestDeltasHandleNilAndEmptyInput(t *testing.T) {
	ctx := context.Background()
	var log deltaLog
	model := modelFunc(func(ctx context.Context, _ []Message, _ []ToolSpec) (Message, error) {
		EmitText(ctx, "")
		EmitToolOutput(ctx, "")
		return Message{Role: RoleAssistant, Text: ""}, nil
	})
	h := openWith(t, ctx, Options{Model: model, LeaseTTL: ttl, OnDelta: log.add})
	defer h.Close()

	sub, err := h.Prompt(ctx, "a", "")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
	require.Empty(t, log.all())
}
