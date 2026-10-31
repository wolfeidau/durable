package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/stretchr/testify/require"
	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/tools"
)

// reply is one scripted HTTP response.
type reply struct {
	status int
	body   string
}

type fakeAPI struct {
	srv *httptest.Server

	mu      sync.Mutex
	replies []reply
	bodies  []string
	headers []http.Header
}

func newFakeAPI(t *testing.T, replies ...reply) *fakeAPI {
	t.Helper()
	f := &fakeAPI{replies: replies}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		i := len(f.bodies)
		f.bodies = append(f.bodies, string(b))
		f.headers = append(f.headers, r.Header.Clone())
		rep := f.replies[min(i, len(f.replies)-1)]
		f.mu.Unlock()

		if rep.status >= 400 {
			w.Header().Set("Content-Type", "application/json")
		} else {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) model(cfg Config) *Model {
	client := anthropic.NewClient(
		option.WithBaseURL(f.srv.URL),
		option.WithAPIKey("test-key"),
		option.WithMaxRetries(0),
	)
	return New(client, cfg)
}

func (f *fakeAPI) body(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Greater(t, len(f.bodies), i)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(f.bodies[i]), &m))
	return m
}

func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func event(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

const messageStart = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}`

func blockStart(i int, block string) string {
	return event("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":%s}`, i, block))
}

func blockDelta(i int, delta string) string {
	return event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":%s}`, i, delta))
}

func blockStop(i int) string {
	return event("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i))
}

func textBlock(i int, parts ...string) string {
	s := blockStart(i, `{"type":"text","text":""}`)
	for _, p := range parts {
		s += blockDelta(i, fmt.Sprintf(`{"type":"text_delta","text":%q}`, p))
	}
	return s + blockStop(i)
}

func thinkingBlock(i int, thinking, sig string) string {
	return blockStart(i, `{"type":"thinking","thinking":"","signature":""}`) +
		blockDelta(i, fmt.Sprintf(`{"type":"thinking_delta","thinking":%q}`, thinking)) +
		blockDelta(i, fmt.Sprintf(`{"type":"signature_delta","signature":%q}`, sig)) +
		blockStop(i)
}

func toolBlock(i int, id, name string, parts ...string) string {
	s := blockStart(i, fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":{}}`, id, name))
	for _, p := range parts {
		s += blockDelta(i, fmt.Sprintf(`{"type":"input_json_delta","partial_json":%q}`, p))
	}
	return s + blockStop(i)
}

func stream(stop string, blocks ...string) reply {
	return streamWithDetails(stop, "null", blocks...)
}

func streamWithDetails(stop, details string, blocks ...string) reply {
	s := event("message_start", messageStart) + strings.Join(blocks, "")
	s += event("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null,"stop_details":%s},"usage":{"output_tokens":5}}`, stop, details))
	s += event("message_stop", `{"type":"message_stop"}`)
	return reply{status: 200, body: s}
}

func apiError(status int, typ string) reply {
	return reply{status: status, body: fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":"boom"}}`, typ)}
}

func gen(t *testing.T, m *Model, history []durable.Message, tools []durable.ToolSpec) (durable.Message, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return m.Generate(ctx, history, tools)
}

func TestGenerateText(t *testing.T) {
	f := newFakeAPI(t, stream("end_turn", textBlock(0, "Hello", ", ", "world")))
	var got []string
	m := f.model(Config{System: "be brief", OnText: func(s string) { got = append(got, s) }})

	msg, err := gen(t, m, []durable.Message{{Role: durable.RoleUser, Text: "hi"}}, nil)
	require.NoError(t, err)
	require.Equal(t, durable.RoleAssistant, msg.Role)
	require.Equal(t, "Hello, world", msg.Text)
	require.Empty(t, msg.ToolCalls)
	require.Equal(t, []string{"Hello", ", ", "world"}, got)
	require.JSONEq(t, `[{"type":"text","text":"Hello, world"}]`, string(msg.Raw))
}

func TestRequestShape(t *testing.T) {
	f := newFakeAPI(t, stream("end_turn", textBlock(0, "ok")))
	m := f.model(Config{System: "be brief"})
	tools := []durable.ToolSpec{
		{Name: "b_tool", Description: "second alphabetically", Schema: json.RawMessage(
			`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)},
		{Name: "a_tool", Description: "no schema"},
	}

	_, err := gen(t, m, []durable.Message{{Role: durable.RoleUser, Text: "hi"}}, tools)
	require.NoError(t, err)

	body := f.body(t, 0)
	require.Equal(t, "claude-sonnet-5-5", body["model"])
	require.EqualValues(t, 64000, body["max_tokens"])
	require.Equal(t, true, body["stream"])
	require.Equal(t, map[string]any{"effort": "medium"}, body["output_config"])
	require.Equal(t, "default", body["fallbacks"])
	require.Equal(t, map[string]any{"type": "ephemeral"}, body["cache_control"])
	require.Equal(t, []any{map[string]any{"type": "text", "text": "be brief"}}, body["system"])
	for _, k := range []string{"thinking", "temperature", "top_p", "top_k", "tool_choice"} {
		require.NotContains(t, body, k)
	}

	require.Contains(t, f.headers[0].Get("anthropic-beta"), "server-side-fallback-2026-07-01")

	toolList := body["tools"].([]any)
	require.Len(t, toolList, 2)
	first := toolList[0].(map[string]any)
	require.Equal(t, "b_tool", first["name"])
	require.Equal(t, true, first["strict"])
	require.NotContains(t, first, "eager_input_streaming")
	require.Equal(t, map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"path": map[string]any{"type": "string"}},
		"required":             []any{"path"},
		"additionalProperties": false,
	}, first["input_schema"])
	require.Equal(t, "a_tool", toolList[1].(map[string]any)["name"])
	require.Equal(t, true, toolList[1].(map[string]any)["strict"])
}

func TestThinkingRawRoundTrip(t *testing.T) {
	const sig = "EqQBCkYIAxgC+/sig=="
	f := newFakeAPI(t,
		stream("tool_use",
			thinkingBlock(0, "let me <think> & check", sig),
			textBlock(1, "reading"),
			toolBlock(2, "toolu_1", "read_file", `{"path":`, `"a.go"}`),
		),
		stream("end_turn", textBlock(0, "done")),
	)
	m := f.model(Config{})
	tools := []durable.ToolSpec{{Name: "read_file"}}
	history := []durable.Message{{Role: durable.RoleUser, Text: "read a.go"}}

	msg, err := gen(t, m, history, tools)
	require.NoError(t, err)
	require.Equal(t, "reading", msg.Text)
	require.Len(t, msg.ToolCalls, 1)
	require.Equal(t, "toolu_1", msg.ToolCalls[0].ID)
	require.Equal(t, "read_file", msg.ToolCalls[0].Name)
	require.JSONEq(t, `{"path":"a.go"}`, string(msg.ToolCalls[0].Input))
	require.Contains(t, string(msg.Raw), `"signature":"`+sig+`"`)
	require.Contains(t, string(msg.Raw), `"thinking":"let me <think> & check"`)

	history = append(history, msg,
		durable.Message{Role: durable.RoleTool, ToolCallID: "toolu_1", Text: "package a"})
	_, err = gen(t, m, history, tools)
	require.NoError(t, err)

	msgs := f.body(t, 1)["messages"].([]any)
	require.Len(t, msgs, 3)
	assistant := msgs[1].(map[string]any)
	require.Equal(t, "assistant", assistant["role"])
	sent, err := json.Marshal(assistant["content"])
	require.NoError(t, err)
	require.JSONEq(t, string(msg.Raw), string(sent))
	require.Contains(t, f.bodies[1], `"signature":"`+sig+`"`)
}

func TestAssistantRebuiltWithoutRaw(t *testing.T) {
	f := newFakeAPI(t, stream("end_turn", textBlock(0, "ok")))
	m := f.model(Config{})
	history := []durable.Message{
		{Role: durable.RoleUser, Text: "go"},
		{Role: durable.RoleAssistant, Text: "calling", ToolCalls: []durable.ToolCall{{ID: "t1", Name: "x", Input: json.RawMessage(`{"a":1}`)}}},
		{Role: durable.RoleTool, ToolCallID: "t1", Text: "res"},
	}
	_, err := gen(t, m, history, nil)
	require.NoError(t, err)

	content := f.body(t, 0)["messages"].([]any)[1].(map[string]any)["content"]
	require.Equal(t, []any{
		map[string]any{"type": "text", "text": "calling"},
		map[string]any{"type": "tool_use", "id": "t1", "name": "x", "input": map[string]any{"a": float64(1)}},
	}, content)
}

func TestParallelToolResultsMerge(t *testing.T) {
	f := newFakeAPI(t, stream("end_turn", textBlock(0, "ok")))
	m := f.model(Config{})
	history := []durable.Message{
		{Role: durable.RoleUser, Text: "go"},
		{Role: durable.RoleAssistant, Raw: json.RawMessage(`[{"type":"tool_use","id":"t1","name":"x","input":{}},{"type":"tool_use","id":"t2","name":"x","input":{}}]`)},
		{Role: durable.RoleTool, ToolCallID: "t1", Text: "one"},
		{Role: durable.RoleTool, ToolCallID: "t2", Text: "two", IsError: true},
	}
	_, err := gen(t, m, history, nil)
	require.NoError(t, err)

	msgs := f.body(t, 0)["messages"].([]any)
	require.Len(t, msgs, 3)
	user := msgs[2].(map[string]any)
	require.Equal(t, "user", user["role"])
	content := user["content"].([]any)
	require.Len(t, content, 2)
	r1, r2 := content[0].(map[string]any), content[1].(map[string]any)
	require.Equal(t, "tool_result", r1["type"])
	require.Equal(t, "t1", r1["tool_use_id"])
	require.Equal(t, "t2", r2["tool_use_id"])
	require.Equal(t, true, r2["is_error"])
}

func TestToolResultThenPromptMerge(t *testing.T) {
	f := newFakeAPI(t, stream("end_turn", textBlock(0, "ok")))
	m := f.model(Config{})
	history := []durable.Message{
		{Role: durable.RoleUser, Text: "go"},
		{Role: durable.RoleAssistant, Raw: json.RawMessage(`[{"type":"tool_use","id":"t1","name":"x","input":{}}]`)},
		{Role: durable.RoleTool, ToolCallID: "t1", Text: "one"},
		{Role: durable.RoleUser, Text: "next prompt"},
	}
	_, err := gen(t, m, history, nil)
	require.NoError(t, err)

	msgs := f.body(t, 0)["messages"].([]any)
	require.Len(t, msgs, 3)
	content := msgs[2].(map[string]any)["content"].([]any)
	require.Len(t, content, 2)
	require.Equal(t, "tool_result", content[0].(map[string]any)["type"])
	require.Equal(t, "text", content[1].(map[string]any)["type"])
	require.Equal(t, "next prompt", content[1].(map[string]any)["text"])
}

func TestStopReasons(t *testing.T) {
	partial := toolBlock(0, "t1", "bash", `{"cmd":"rm`)
	tests := []struct {
		name      string
		reply     reply
		wantErr   string
		wantCalls int
	}{
		{name: "end_turn", reply: stream("end_turn", textBlock(0, "x"))},
		{name: "tool_use", reply: stream("tool_use", toolBlock(0, "t1", "bash", `{"cmd":"ls"}`)), wantCalls: 1},
		{name: "stop_sequence", reply: stream("stop_sequence", textBlock(0, "x"))},
		{name: "max_tokens", reply: stream("max_tokens", partial), wantErr: `"max_tokens"`},
		{name: "pause_turn", reply: stream("pause_turn", partial), wantErr: `"pause_turn"`},
		{name: "unknown", reply: stream("brand_new", partial), wantErr: `"brand_new"`},
		{
			name:    "refusal",
			reply:   streamWithDetails("refusal", `{"type":"refusal","category":"cyber","explanation":"nope"}`, partial),
			wantErr: "cyber",
		},
		{name: "refusal without details", reply: stream("refusal"), wantErr: "refused"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t, tc.reply)
			msg, err := gen(t, f.model(Config{}), []durable.Message{{Role: durable.RoleUser, Text: "hi"}}, nil)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Empty(t, msg.ToolCalls)
				require.Empty(t, msg.Raw)
				return
			}
			require.NoError(t, err)
			require.Len(t, msg.ToolCalls, tc.wantCalls)
		})
	}
}

func TestContextCancelMidStream(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, event("message_start", messageStart))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	m := New(anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("k"), option.WithMaxRetries(0)), Config{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	_, err := m.Generate(ctx, []durable.Message{{Role: durable.RoleUser, Text: "hi"}}, nil)
	require.ErrorIs(t, err, context.Canceled)
}

// runThroughHarness drives one prompt and reports whether the model error was
// treated as transient (submission left running, Run errors) or fatal.
func runThroughHarness(t *testing.T, f *fakeAPI) (sub durable.Submission, runErr error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	h, err := durable.Open(ctx, filepath.Join(t.TempDir(), "session.sqlite"), durable.Options{
		Model:         f.model(Config{}),
		ModelAttempts: 2,
		ModelBackoff:  time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	_, err = h.Submit(ctx, "req-1", "hi")
	require.NoError(t, err)
	runErr = h.Run(ctx)
	sub, err = h.Get(ctx, "req-1")
	require.NoError(t, err)
	return sub, runErr
}

func TestErrorClassification(t *testing.T) {
	transient := []int{408, 409, 429, 500, 502, 503, 529}
	fatal := []int{400, 401, 403, 404, 413, 422}

	for _, status := range transient {
		t.Run(fmt.Sprintf("transient_%d", status), func(t *testing.T) {
			f := newFakeAPI(t, apiError(status, "api_error"))
			sub, err := runThroughHarness(t, f)
			require.Error(t, err)
			require.Equal(t, durable.StatusRunning, sub.Status)
			require.Equal(t, 2, f.count())
		})
	}
	for _, status := range fatal {
		t.Run(fmt.Sprintf("fatal_%d", status), func(t *testing.T) {
			f := newFakeAPI(t, apiError(status, "invalid_request_error"))
			sub, err := runThroughHarness(t, f)
			require.NoError(t, err)
			require.Equal(t, durable.StatusFailed, sub.Status)
			require.Equal(t, 1, f.count())
		})
	}
}

func TestStreamErrorClassification(t *testing.T) {
	sseErr := func(typ string) reply {
		return reply{status: 200, body: event("message_start", messageStart) +
			event("error", fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":"boom"}}`, typ))}
	}
	for _, typ := range []string{"overloaded_error", "rate_limit_error", "api_error"} {
		t.Run(typ, func(t *testing.T) {
			sub, err := runThroughHarness(t, newFakeAPI(t, sseErr(typ)))
			require.Error(t, err)
			require.Equal(t, durable.StatusRunning, sub.Status)
		})
	}
	t.Run("invalid_request_error", func(t *testing.T) {
		sub, err := runThroughHarness(t, newFakeAPI(t, sseErr("invalid_request_error")))
		require.NoError(t, err)
		require.Equal(t, durable.StatusFailed, sub.Status)
	})
	t.Run("cut stream", func(t *testing.T) {
		f := newFakeAPI(t, reply{status: 200, body: event("message_start", messageStart) + textBlock(0, "par")})
		sub, err := runThroughHarness(t, f)
		require.Error(t, err)
		require.Equal(t, durable.StatusRunning, sub.Status)
	})
	t.Run("connection refused", func(t *testing.T) {
		f := newFakeAPI(t, stream("end_turn"))
		f.srv.Close()
		sub, err := runThroughHarness(t, f)
		require.Error(t, err)
		require.Equal(t, durable.StatusRunning, sub.Status)
	})
}

func TestSmoke(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" || os.Getenv("DURABLE_SMOKE") != "1" {
		t.Skip("set ANTHROPIC_API_KEY and DURABLE_SMOKE=1 to run against the real API")
	}
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "note.txt"), []byte("the word is pong\n"), 0o600))
	toolset, err := tools.All(root)
	require.NoError(t, err)
	specs := make([]durable.ToolSpec, len(toolset))
	for i, tool := range toolset {
		specs[i] = tool.Spec()
	}

	// Sends the real strict schemas, then replays the first turn's thinking and tool call.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	m := New(anthropic.NewClient(), Config{MaxTokens: 8000})
	history := []durable.Message{{Role: durable.RoleUser, Text: "Read note.txt with read_file, then reply with only the word in it."}}
	first, err := m.Generate(ctx, history, specs)
	require.NoError(t, err)
	require.NotEmpty(t, first.Raw)
	require.NotEmpty(t, first.ToolCalls)

	history = append(history, first)
	for _, call := range first.ToolCalls {
		out, err := toolset[0].Execute(ctx, call)
		require.NoError(t, err)
		history = append(history, durable.Message{Role: durable.RoleTool, ToolCallID: call.ID, Text: out})
	}
	second, err := m.Generate(ctx, history, specs)
	require.NoError(t, err)
	require.Contains(t, strings.ToLower(second.Text), "pong")
}
