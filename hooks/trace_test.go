package hooks

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/wolfeidau/durable"
)

type toolModel struct{}

func (toolModel) Generate(_ context.Context, history []durable.Message, _ []durable.ToolSpec) (durable.Message, error) {
	if history[len(history)-1].Role == durable.RoleUser {
		return durable.Message{Role: durable.RoleAssistant, ToolCalls: []durable.ToolCall{bashCall}}, nil
	}
	return durable.Message{Role: durable.RoleAssistant, Text: "done"}, nil
}

func TestHookSpanAndRedactedResultOnToolSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	body := `echo '{"hookSpecificOutput":{"updatedToolOutput":"redacted"}}'`
	r := New(load(t, PostToolUse, "bash", handlerJSON(t, script(t, body), nil)), Options{TracerProvider: tp})

	ctx := context.Background()
	h, err := durable.Open(ctx, filepath.Join(t.TempDir(), "s.sqlite"), durable.Options{
		Model: toolModel{}, Tools: r.Wrap([]durable.Tool{fakeTool{name: "bash", out: "secret-token"}}),
		TracerProvider: tp, CaptureContent: true,
	})
	require.NoError(t, err)
	defer h.Close()
	_, err = h.Prompt(ctx, "r1", "go")
	require.NoError(t, err)

	var tool, hook sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		switch s.Name() {
		case "execute_tool bash":
			tool = s
		case "durable.hook":
			hook = s
		}
	}
	require.NotNil(t, tool)
	require.NotNil(t, hook)
	require.Equal(t, tool.SpanContext().SpanID(), hook.Parent().SpanID())
	require.Equal(t, codes.Unset, hook.Status().Code)

	got := map[string]string{}
	for _, kv := range hook.Attributes() {
		got[string(kv.Key)] = kv.Value.String()
	}
	require.Equal(t, PostToolUse, got["durable.hook.event"])
	require.Equal(t, "bash", got["durable.hook.tool"])
	require.Equal(t, "0", got["durable.hook.exit_code"])

	for _, kv := range tool.Attributes() {
		if kv.Key == "gen_ai.tool.call.result" {
			require.Equal(t, "redacted", kv.Value.AsString())
			return
		}
	}
	require.FailNow(t, "tool span has no result")
}

func TestHookFailureSpanHasNoCommandText(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	r := New(load(t, PreToolUse, "bash", handlerJSON(t, script(t, `echo boom >&2; exit 1`), nil)), Options{TracerProvider: tp})
	d, err := r.Gate(nil)(context.Background(), durable.ToolCall{ID: "c1", Name: "bash", Input: json.RawMessage(`{}`)})
	require.NoError(t, err)
	require.Equal(t, durable.ActionDeny, d.Action)

	spans := rec.Ended()
	require.Len(t, spans, 1)
	require.Equal(t, codes.Error, spans[0].Status().Code)
	require.Equal(t, "hook failed", spans[0].Status().Description)
}
