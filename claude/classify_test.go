package claude

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/fakeapi"
)

func kind(err error) reflect.Type { return reflect.TypeOf(err) }

func TestOverflowClassification(t *testing.T) {
	transient := kind(durable.Transient(errors.New("x")))
	overflow := kind(durable.ContextOverflow(errors.New("x")))
	plain := kind(fmt.Errorf("x: %w", errors.New("y")))

	errBody := func(typ, msg string) string {
		return fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":%q}}`, typ, msg)
	}
	sse := func(typ, msg string) reply {
		return reply{status: 200, body: messageStart + fakeapi.Event("error", errBody(typ, msg))}
	}
	cases := []struct {
		name  string
		reply reply
		want  reflect.Type
	}{
		{"400 prompt too long", reply{400, errBody("invalid_request_error", "prompt is too long: 250000 tokens > 200000 maximum")}, overflow},
		{"413 request_too_large", reply{413, errBody("request_too_large", "Request exceeds the maximum size")}, overflow},
		{"413 any text", reply{413, errBody("invalid_request_error", "boom")}, overflow},
		{"400 unrelated", reply{400, errBody("invalid_request_error", "boom")}, plain},
		{"429 too many tokens", reply{429, errBody("rate_limit_error", "too many tokens")}, transient},
		{"500 overflow text", reply{500, errBody("api_error", "prompt is too long")}, transient},
		{"sse invalid request overflow", sse("invalid_request_error", "prompt is too long"), overflow},
		{"sse invalid request unrelated", sse("invalid_request_error", "boom"), plain},
		{"sse overloaded overflow text", sse("overloaded_error", "prompt is too long"), transient},
		{"sse rate limit", sse("rate_limit_error", "too many tokens"), transient},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := gen(t, newFakeAPI(t, c.reply).model(Config{}), []durable.Message{{Role: durable.RoleUser, Text: "hi"}}, nil)
			require.Error(t, err)
			require.Equal(t, c.want, kind(err), err.Error())
			require.Contains(t, err.Error(), "failed to generate: ")
		})
	}
}

func TestContextTokens(t *testing.T) {
	blocks := fakeapi.TextBlock(0, "hi")
	cases := []struct {
		name         string
		start, delta string
		want         int64
	}{
		// message_delta overwrites output_tokens and keeps input and cache counts from message_start.
		{"start input and cache, delta output",
			`{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":1000,"output_tokens":1}`, `{"output_tokens":7}`, 1117},
		{"delta repeats inputs",
			`{"input_tokens":10,"cache_read_input_tokens":100,"output_tokens":1}`, `{"input_tokens":10,"cache_read_input_tokens":100,"output_tokens":7}`, 117},
		{"no usage", `{}`, `{}`, 0},
		{"null usage", `null`, `null`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := fakeapi.StreamUsage("claude-sonnet-5-5", "end_turn", "null", c.start, c.delta, blocks)
			msg, err := gen(t, newFakeAPI(t, reply{status: 200, body: body}).model(Config{}), []durable.Message{{Role: durable.RoleUser, Text: "hi"}}, nil)
			require.NoError(t, err)
			require.Equal(t, "hi", msg.Text)
			require.Equal(t, c.want, msg.ContextTokens)
		})
	}

	// The shared helper's usage is input 1 plus delta output 5.
	msg, err := gen(t, newFakeAPI(t, stream("end_turn", blocks)).model(Config{}), []durable.Message{{Role: durable.RoleUser, Text: "hi"}}, nil)
	require.NoError(t, err)
	require.Equal(t, int64(6), msg.ContextTokens)
}

func TestHeaderTimeoutIsTransientButCancellationIsNot(t *testing.T) {
	timeout := fmt.Errorf("timeout awaiting response headers: %w", context.DeadlineExceeded)
	transient := reflect.TypeOf(durable.Transient(errors.New("x")))

	require.Equal(t, transient, reflect.TypeOf(classify(context.Background(), timeout)))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NotEqual(t, transient, reflect.TypeOf(classify(ctx, timeout)))
}

func TestContextWindowStopReasonIsOverflow(t *testing.T) {
	body := fakeapi.Stream("claude-sonnet-5-5", "model_context_window_exceeded", "null", fakeapi.TextBlock(0, "partial"))
	_, err := gen(t, newFakeAPI(t, reply{status: 200, body: body}).model(Config{}), []durable.Message{{Role: durable.RoleUser, Text: "hi"}}, nil)
	require.Equal(t, kind(durable.ContextOverflow(errors.New("x"))), kind(err))
}
