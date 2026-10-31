package durable

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type recoverCounter struct {
	n         atomic.Int32
	note      string
	onRecover func(ctx context.Context, c ToolCall)
}

func (r *recoverCounter) Recover(ctx context.Context, c ToolCall) string {
	r.n.Add(1)
	if r.onRecover != nil {
		r.onRecover(ctx, c)
	}
	return r.note
}

type recFake struct {
	*fakeTool
	recoverCounter
}

type recBlocking struct {
	*blockingTool
	recoverCounter
}

func callStatusOf(t *testing.T, h *Harness, id string) string {
	t.Helper()
	var s string
	require.NoError(t, h.db.QueryRow(`SELECT status FROM tool_calls WHERE call_id = ?`, id).Scan(&s))
	return s
}

func TestRecoverRunsOnceBeforeResultCommits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	model := &scriptedModel{}
	crashCtx, cancel := context.WithCancel(context.Background())
	write := &recFake{fakeTool: &fakeTool{name: "write_file", crashOn: 1, cancel: cancel}, note: "orphan stopped."}
	read := &recFake{fakeTool: &fakeTool{name: "read_file", replay: ReplaySafe}, note: "unused"}
	h1 := open(t, crashCtx, path, model, write, read)
	_, err := h1.Prompt(crashCtx, "req-1", "go")
	require.ErrorIs(t, err, context.Canceled)
	h1.abandon()
	require.Zero(t, write.n.Load())

	ctx := context.Background()
	var h2 *Harness
	var statusInRecover string
	var messagesInRecover int
	write.onRecover = func(ctx context.Context, _ ToolCall) {
		statusInRecover = callStatusOf(t, h2, "call_write")
		hist, err := h2.History(ctx)
		require.NoError(t, err)
		messagesInRecover = len(hist)
	}
	h2 = open(t, ctx, path, model, write, read)
	defer h2.Close()
	require.NoError(t, h2.Run(ctx))

	require.EqualValues(t, 1, write.n.Load())
	require.Zero(t, read.n.Load(), "pending calls are not recovered")
	require.Equal(t, "started", statusInRecover)
	require.Equal(t, 2, messagesInRecover)
	hist, err := h2.History(ctx)
	require.NoError(t, err)
	require.Equal(t, UnknownOutcome+" orphan stopped.", hist[2].Text)
	require.True(t, hist[2].IsError)
}

func TestRecoverNotCalledForReplaySafeStartedCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	model := &scriptedModel{}
	crashCtx, cancel := context.WithCancel(context.Background())
	write := &recFake{fakeTool: &fakeTool{name: "write_file"}}
	read := &recFake{fakeTool: &fakeTool{name: "read_file", replay: ReplaySafe, crashOn: 1, cancel: cancel}}
	h1 := open(t, crashCtx, path, model, write, read)
	_, err := h1.Prompt(crashCtx, "req-1", "go")
	require.ErrorIs(t, err, context.Canceled)
	h1.abandon()

	ctx := context.Background()
	h2 := open(t, ctx, path, model, write, read)
	defer h2.Close()
	require.NoError(t, h2.Run(ctx))
	require.EqualValues(t, 2, read.runs.Load())
	require.Zero(t, read.n.Load())
	require.Zero(t, write.n.Load())
}

func TestCancelDuringRecoverLeavesCallStarted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	model := modelFunc(func(_ context.Context, h []Message, s []ToolSpec) (Message, error) { return approvalModel(h, s) })
	crashCtx, cancel := context.WithCancel(context.Background())
	write := &recFake{fakeTool: &fakeTool{name: "write_file", crashOn: 1, cancel: cancel}}
	h1 := open(t, crashCtx, path, model, write)
	_, _ = h1.Prompt(crashCtx, "a", "go")
	h1.abandon()

	ctx := context.Background()
	runCtx, stop := context.WithCancel(ctx)
	write.onRecover = func(context.Context, ToolCall) { stop() }
	write.note = "stopped."
	h2 := open(t, ctx, path, model, write)
	defer h2.Close()
	require.ErrorIs(t, h2.Run(runCtx), context.Canceled)
	require.Equal(t, "started", callStatus(t, h2))
	hist, err := h2.History(ctx)
	require.NoError(t, err)
	require.Len(t, hist, 2)

	write.onRecover = nil
	require.NoError(t, h2.Run(ctx))
	hist, err = h2.History(ctx)
	require.NoError(t, err)
	require.Equal(t, UnknownOutcome+" stopped.", hist[2].Text)
	require.EqualValues(t, 2, write.n.Load())
}

func TestCancelRecoversStartedCallsOnly(t *testing.T) {
	ctx := context.Background()
	a := &recFake{fakeTool: &fakeTool{name: "a", replay: ReplaySafe}}
	b := &recBlocking{blockingTool: newBlockingTool("b")}
	b.note = "b stopped."
	c := &recFake{fakeTool: &fakeTool{name: "c"}}
	h := openWith(t, ctx, Options{LeaseTTL: ttl, Model: modelFunc(threeCallModel), Tools: []Tool{a, b, c}})
	defer h.Close()

	_, err := h.Submit(ctx, "r1", "go")
	require.NoError(t, err)
	stop := startRun(h)
	<-b.entered
	require.ErrorIs(t, stop(), context.Canceled)
	require.Zero(t, b.n.Load())

	_, err = h.Cancel(ctx, "r1")
	require.NoError(t, err)
	require.EqualValues(t, 1, b.n.Load())
	require.Zero(t, a.n.Load())
	require.Zero(t, c.n.Load())

	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.Equal(t, CancelledUnknown+" b stopped.", hist[3].Text)
	require.Equal(t, CallCancelled, hist[4].Text)

	_, err = h.Cancel(ctx, "r1")
	require.NoError(t, err)
	require.EqualValues(t, 1, b.n.Load())
}
