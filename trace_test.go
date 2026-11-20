package durable

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func newRecorder() (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	rec := tracetest.NewSpanRecorder()
	return rec, sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
}

func spanAttrs(s sdktrace.ReadOnlySpan) map[string]attribute.Value {
	m := map[string]attribute.Value{}
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

func findSpan(t *testing.T, rec *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range rec.Ended() {
		if s.Name() == name {
			return s
		}
	}
	require.FailNow(t, "span not found", name)
	return nil
}

var contentKeys = []string{
	"gen_ai.input.messages", "gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "durable.content_truncated",
}

func TestTraceSubmissionWithOneToolCall(t *testing.T) {
	ctx := context.Background()
	rec, tp := newRecorder()
	write := &fakeTool{name: "write_file"}
	h := openWith(t, ctx, Options{Model: &scriptedModel{}, LeaseTTL: ttl, Tools: []Tool{write, &fakeTool{name: "read_file"}}, TracerProvider: tp})
	defer h.Close()
	sub, err := h.Prompt(ctx, "req-1", "secret prompt")
	require.NoError(t, err)

	agent := findSpan(t, rec, "invoke_agent durable")
	a := spanAttrs(agent)
	require.Equal(t, "invoke_agent", a["gen_ai.operation.name"].AsString())
	require.Equal(t, "durable", a["gen_ai.agent.name"].AsString())
	require.Equal(t, "req-1", a["durable.request_id"].AsString())
	require.Equal(t, sub.Seq, a["durable.submission.seq"].AsInt64())
	require.Equal(t, string(StatusDone), a["durable.submission.status"].AsString())
	require.Equal(t, codes.Unset, agent.Status().Code)

	tool := findSpan(t, rec, "execute_tool write_file")
	require.Equal(t, agent.SpanContext().SpanID(), tool.Parent().SpanID())
	ta := spanAttrs(tool)
	require.Equal(t, "execute_tool", ta["gen_ai.operation.name"].AsString())
	require.Equal(t, "write_file", ta["gen_ai.tool.name"].AsString())
	require.Equal(t, "call_write", ta["gen_ai.tool.call.id"].AsString())
	require.Equal(t, "allow", ta["durable.tool.decision"].AsString())
	require.False(t, ta["durable.replayed"].AsBool())
	require.False(t, ta["durable.rewritten"].AsBool())

	for _, s := range rec.Ended() {
		for _, k := range contentKeys {
			require.NotContains(t, spanAttrs(s), k, s.Name())
		}
	}
}

func TestTraceCapturesContent(t *testing.T) {
	ctx := context.Background()
	rec, tp := newRecorder()
	h := openWith(t, ctx, Options{
		Model: &scriptedModel{}, LeaseTTL: ttl, TracerProvider: tp, CaptureContent: true,
		Tools: []Tool{&fakeTool{name: "write_file"}, &fakeTool{name: "read_file"}},
		Gate:  rewriteGate(`{"path":"b.txt"}`),
	})
	defer h.Close()
	_, err := h.Prompt(ctx, "req-1", "fix it")
	require.NoError(t, err)

	require.JSONEq(t, `[{"role":"user","parts":[{"type":"text","content":"fix it"}]}]`,
		spanAttrs(findSpan(t, rec, "invoke_agent durable"))["gen_ai.input.messages"].AsString())
	ta := spanAttrs(findSpan(t, rec, "execute_tool write_file"))
	require.JSONEq(t, `{"path":"a.txt"}`, ta["gen_ai.tool.call.arguments"].AsString())
	require.Equal(t, "write_file ok (run 1)", ta["gen_ai.tool.call.result"].AsString())
	require.True(t, ta["durable.rewritten"].AsBool())
	require.NotContains(t, ta, "durable.content_truncated")
	requireNoAttrContains(t, rec, "b.txt")
}

func requireNoAttrContains(t *testing.T, rec *tracetest.SpanRecorder, secret string) {
	t.Helper()
	for _, s := range rec.Ended() {
		for _, kv := range s.Attributes() {
			require.NotContains(t, kv.Value.String(), secret, "%s %s", s.Name(), kv.Key)
		}
	}
}

func TestTraceOmitsRewrittenInputOnReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	model := &scriptedModel{}
	write := &fakeTool{name: "write_file", replay: ReplayNever}
	read := &fakeTool{name: "read_file", replay: ReplaySafe, crashOn: 1}

	crashCtx, cancel := context.WithCancel(context.Background())
	read.cancel = cancel
	h1, err := Open(crashCtx, path, Options{
		Model: model, Tools: []Tool{write, read}, LeaseTTL: ttl, Gate: rewriteGate(`{"path":"secret.txt"}`),
	})
	require.NoError(t, err)
	_, _ = h1.Prompt(crashCtx, "req-1", "fix the bug")
	h1.abandon()

	ctx := context.Background()
	rec, tp := newRecorder()
	h2, err := Open(ctx, path, Options{
		Model: model, Tools: []Tool{write, read}, LeaseTTL: ttl, TracerProvider: tp, CaptureContent: true,
	})
	require.NoError(t, err)
	defer h2.Close()
	require.NoError(t, h2.Run(ctx))

	a := spanAttrs(findSpan(t, rec, "execute_tool read_file"))
	require.True(t, a["durable.replayed"].AsBool())
	require.True(t, a["durable.rewritten"].AsBool())
	require.NotContains(t, a, "gen_ai.tool.call.arguments")
	requireNoAttrContains(t, rec, "secret.txt")
}

type bigTool struct{ fakeTool }

func (*bigTool) Execute(context.Context, ToolCall) (string, error) {
	return strings.Repeat("é", 10<<10), nil // 20 KiB
}

func TestTraceTruncatesContent(t *testing.T) {
	ctx := context.Background()
	rec, tp := newRecorder()
	h := openWith(t, ctx, Options{
		Model: &scriptedModel{}, LeaseTTL: ttl, TracerProvider: tp, CaptureContent: true,
		Tools: []Tool{&bigTool{fakeTool{name: "write_file"}}, &fakeTool{name: "read_file"}},
	})
	defer h.Close()
	_, err := h.Prompt(ctx, "req-1", "go")
	require.NoError(t, err)

	ta := spanAttrs(findSpan(t, rec, "execute_tool write_file"))
	require.Len(t, ta["gen_ai.tool.call.result"].AsString(), 16<<10)
	require.True(t, ta["durable.content_truncated"].AsBool())
	ra := spanAttrs(findSpan(t, rec, "execute_tool read_file"))
	require.NotContains(t, ra, "durable.content_truncated")
}

func TestTraceParkedApprovalIsNotAnError(t *testing.T) {
	ctx := context.Background()
	rec, tp := newRecorder()
	opts := deferOpts(gateModel(), nil, &fakeTool{name: "write_file"})
	opts.TracerProvider = tp
	h := openWith(t, ctx, opts)
	defer h.Close()
	parkOne(t, ctx, h)

	tool := findSpan(t, rec, "execute_tool write_file")
	require.Equal(t, "parked", spanAttrs(tool)["durable.tool.decision"].AsString())
	require.Equal(t, codes.Unset, tool.Status().Code)
	agent := findSpan(t, rec, "invoke_agent durable")
	require.Equal(t, codes.Unset, agent.Status().Code)
	require.Equal(t, string(StatusRunning), spanAttrs(agent)["durable.submission.status"].AsString())
}

func TestTraceBlockedCallIsNotAnError(t *testing.T) {
	ctx := context.Background()
	rec, tp := newRecorder()
	h := openWith(t, ctx, Options{Model: gateModel(), LeaseTTL: ttl, TracerProvider: tp,
		Tools: []Tool{&fakeTool{name: "write_file"}}, Gate: func(context.Context, ToolCall) (Decision, error) {
			return Decision{Action: ActionDeny}, nil
		}})
	defer h.Close()
	_, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)
	tool := findSpan(t, rec, "execute_tool write_file")
	require.Equal(t, "blocked", spanAttrs(tool)["durable.tool.decision"].AsString())
	require.Equal(t, codes.Unset, tool.Status().Code)
}

func TestTraceReplayedAfterCrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	model := &scriptedModel{}
	write := &fakeTool{name: "write_file", replay: ReplayNever}
	read := &fakeTool{name: "read_file", replay: ReplaySafe, crashOn: 1}

	crashCtx, cancel := context.WithCancel(context.Background())
	read.cancel = cancel
	rec1, tp1 := newRecorder()
	h1, err := Open(crashCtx, path, Options{Model: model, Tools: []Tool{write, read}, LeaseTTL: ttl, TracerProvider: tp1})
	require.NoError(t, err)
	_, _ = h1.Prompt(crashCtx, "req-1", "fix the bug")
	h1.abandon()
	// A cancelled call is the process going away, not a failure.
	require.Equal(t, codes.Unset, findSpan(t, rec1, "execute_tool read_file").Status().Code)

	ctx := context.Background()
	rec, tp := newRecorder()
	h2, err := Open(ctx, path, Options{Model: model, Tools: []Tool{write, read}, LeaseTTL: ttl, TracerProvider: tp})
	require.NoError(t, err)
	defer h2.Close()
	require.NoError(t, h2.Run(ctx))

	a := spanAttrs(findSpan(t, rec, "execute_tool read_file"))
	require.True(t, a["durable.replayed"].AsBool())
	require.Equal(t, "allow", a["durable.tool.decision"].AsString())
}

func TestTraceCompactOutcome(t *testing.T) {
	ctx := context.Background()
	rec, tp := newRecorder()
	h := openWith(t, ctx, Options{
		Model: modelFunc(okReply2), LeaseTTL: ttl, TracerProvider: tp,
		Compactor: compactorFunc(func(context.Context, CompactRequest) (Compaction, bool, error) {
			return Compaction{}, false, nil
		}),
	})
	defer h.Close()
	_, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)

	a := spanAttrs(findSpan(t, rec, "durable.compact"))
	require.Equal(t, "skipped", a["durable.compact.outcome"].AsString())
	require.False(t, a["durable.compact.forced"].AsBool())
}

type errTool struct{ fakeTool }

func (*errTool) Execute(context.Context, ToolCall) (string, error) {
	return "", errors.New("disk on fire")
}

func TestTraceToolErrorSetsStatusWithoutLeakingText(t *testing.T) {
	ctx := context.Background()
	rec, tp := newRecorder()
	h := openWith(t, ctx, Options{Model: &scriptedModel{}, LeaseTTL: ttl, TracerProvider: tp,
		Tools: []Tool{&errTool{fakeTool{name: "write_file"}}, &fakeTool{name: "read_file"}}})
	defer h.Close()
	_, err := h.Prompt(ctx, "req-1", "go")
	require.NoError(t, err)

	tool := findSpan(t, rec, "execute_tool write_file")
	require.Equal(t, codes.Error, tool.Status().Code)
	require.NotContains(t, tool.Status().Description, "disk on fire")
}

func okReply2(context.Context, []Message, []ToolSpec) (Message, error) {
	return Message{Role: RoleAssistant, Text: "ok"}, nil
}
