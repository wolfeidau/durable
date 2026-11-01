package responses

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/fakeapi"
)

type reply struct {
	status int
	body   string
}

type fakeServer struct {
	srv *httptest.Server

	mu      sync.Mutex
	replies []reply
	paths   []string
	bodies  []string
	headers []http.Header
}

func newServer(t *testing.T, replies ...reply) *fakeServer {
	t.Helper()
	f := &fakeServer{replies: replies}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		i := len(f.bodies)
		f.bodies = append(f.bodies, string(b))
		f.paths = append(f.paths, r.URL.Path)
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

func ok(body string) reply { return reply{status: 200, body: body} }

// stubReply is for tests that only inspect the request.
func stubReply() reply { return ok(completed(fakeapi.ResponsesMessageItem("msg_stub", "ok"))) }

func (f *fakeServer) model(cfg Config) *Model {
	cfg.BaseURL = f.srv.URL + "/v1"
	if cfg.APIKey == "" {
		cfg.APIKey = "test-key"
	}
	if cfg.Model == "" {
		cfg.Model = "test/model"
	}
	return New(cfg)
}

func (f *fakeServer) body(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(f.bodies[i]), &m))
	return m
}

var prompt = []durable.Message{{Role: durable.RoleUser, Text: "hi"}}

func generate(t *testing.T, f *fakeServer, cfg Config) (durable.Message, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return f.model(cfg).Generate(ctx, prompt, nil)
}

func TestDefaultClientBoundsTheHeaderWait(t *testing.T) {
	transport, ok := DefaultHTTPClient().Transport.(*http.Transport)
	require.True(t, ok)
	require.Equal(t, responseHeaderTimeout, transport.ResponseHeaderTimeout)
	require.Zero(t, DefaultHTTPClient().Timeout, "a total timeout would cut long streams")
}

func TestStalledHeadersAreTransient(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	m := New(Config{BaseURL: srv.URL, HTTPClient: &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 20 * time.Millisecond}}})
	_, err := m.Generate(context.Background(), []durable.Message{{Role: durable.RoleUser, Text: "hi"}}, nil)
	require.Error(t, err)
	require.True(t, isTransient(err), err.Error())
}

func isTransient(err error) bool {
	return reflect.TypeOf(err) == reflect.TypeOf(durable.Transient(errors.New("x")))
}

func completed(items ...string) string {
	return fakeapi.ResponsesCompleted("[" + strings.Join(items, ",") + "]")
}

func TestText(t *testing.T) {
	msg := fakeapi.ResponsesMessageItem("msg_1", "hello world")
	f := newServer(t, ok(
		fakeapi.ResponsesItemAdded(0, `{"type":"message","id":"msg_1","role":"assistant","content":[]}`)+
			fakeapi.ResponsesTextDelta(0, "hello")+fakeapi.ResponsesTextDelta(0, " wor")+fakeapi.ResponsesTextDelta(0, "ld")+
			fakeapi.ResponsesItemDone(0, msg)+completed(msg)))
	var got []string
	out, err := generate(t, f, Config{OnText: func(s string) { got = append(got, s) }})
	require.NoError(t, err)
	require.Equal(t, durable.RoleAssistant, out.Role)
	require.Equal(t, "hello world", out.Text)
	require.Empty(t, out.ToolCalls)
	require.Equal(t, []string{"hello", " wor", "ld"}, got)
	require.JSONEq(t, "["+msg+"]", string(out.Raw))
	require.Equal(t, "/v1/responses", f.paths[0])
	require.Equal(t, "Bearer test-key", f.headers[0].Get("Authorization"))
}

func TestBaseURLTrailingSlash(t *testing.T) {
	f := newServer(t, stubReply())
	m := New(Config{BaseURL: f.srv.URL + "/v1/", APIKey: "k", Model: "m"})
	_, err := m.Generate(context.Background(), prompt, nil)
	require.NoError(t, err)
	require.Equal(t, "/v1/responses", f.paths[0])
}

func TestToolCallsUseCallID(t *testing.T) {
	a := fakeapi.ResponsesCallItem("fc_a", "call_a", "bash", `{"command":"echo a"}`)
	b := fakeapi.ResponsesCallItem("fc_b", "call_b", "read", `{"path":"x"}`)
	f := newServer(t, ok(fakeapi.ResponsesItem(0, a)+fakeapi.ResponsesItem(1, b)+completed(a, b)))
	out, err := generate(t, f, Config{})
	require.NoError(t, err)
	require.Equal(t, []durable.ToolCall{
		{ID: "call_a", Name: "bash", Input: json.RawMessage(`{"command":"echo a"}`)},
		{ID: "call_b", Name: "read", Input: json.RawMessage(`{"path":"x"}`)},
	}, out.ToolCalls)
}

func TestItemsOrderedByOutputIndex(t *testing.T) {
	a := fakeapi.ResponsesCallItem("fc_a", "call_a", "bash", `{}`)
	b := fakeapi.ResponsesCallItem("fc_b", "call_b", "bash", `{}`)
	f := newServer(t, ok(
		fakeapi.ResponsesItemAdded(0, a)+fakeapi.ResponsesItemAdded(1, b)+
			fakeapi.ResponsesItemDone(1, b)+fakeapi.ResponsesItemDone(0, a)+completed()))
	out, err := generate(t, f, Config{})
	require.NoError(t, err)
	require.Equal(t, "call_a", out.ToolCalls[0].ID)
	require.Equal(t, "call_b", out.ToolCalls[1].ID)
}

func TestCommentLinesAndCRLF(t *testing.T) {
	msg := fakeapi.ResponsesMessageItem("msg_1", "hi")
	body := ": keepalive\n\n" + fakeapi.ResponsesItem(0, msg) + ": OPENROUTER PROCESSING\n\n" + completed(msg)
	f := newServer(t, ok(strings.ReplaceAll(body, "\n", "\r\n")))
	out, err := generate(t, f, Config{})
	require.NoError(t, err)
	require.Equal(t, "hi", out.Text)
}

func TestLargeItem(t *testing.T) {
	big := strings.Repeat("x", 3<<20)
	msg := fakeapi.ResponsesMessageItem("msg_1", big)
	f := newServer(t, ok(fakeapi.ResponsesItem(0, msg)+completed(msg)))
	out, err := generate(t, f, Config{})
	require.NoError(t, err)
	require.Len(t, out.Text, len(big))
}

func TestToolArguments(t *testing.T) {
	good := map[string]string{
		"empty":      ``,
		"whitespace": `  `,
		"null":       `null`,
		"empty obj":  `{}`,
	}
	for name, args := range good {
		t.Run(name, func(t *testing.T) {
			c := fakeapi.ResponsesCallItem("fc_1", "call_1", "bash", args)
			f := newServer(t, ok(fakeapi.ResponsesItem(0, c)+completed(c)))
			out, err := generate(t, f, Config{})
			require.NoError(t, err)
			require.JSONEq(t, `{}`, string(out.ToolCalls[0].Input))
		})
	}
	bad := map[string]string{
		"truncated": `{"command":"rm -rf`,
		"array":     `[1]`,
		"string":    `"x"`,
		"number":    `5`,
		"bool":      `true`,
		"garbage":   `nope`,
	}
	for name, args := range bad {
		t.Run(name, func(t *testing.T) {
			c := fakeapi.ResponsesCallItem("fc_1", "call_1", "bash", args)
			f := newServer(t, ok(fakeapi.ResponsesItem(0, c)+completed(c)))
			out, err := generate(t, f, Config{})
			require.Error(t, err)
			require.False(t, isTransient(err))
			require.Empty(t, out.ToolCalls)
		})
	}
}

func TestReasoningReplayedByteForByte(t *testing.T) {
	rs := `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"a<b>&c"}`
	c := fakeapi.ResponsesCallItem("fc_1", "call_1", "bash", `{"command":"ls"}`)
	f := newServer(t, ok(fakeapi.ResponsesItem(0, rs)+fakeapi.ResponsesItem(1, c)+completed(rs, c)), stubReply())
	out, err := generate(t, f, Config{})
	require.NoError(t, err)
	require.Equal(t, "["+rs+","+c+"]", string(out.Raw))

	history := []durable.Message{
		{Role: durable.RoleUser, Text: "hi"},
		out,
		{Role: durable.RoleTool, ToolCallID: "call_1", Text: "done"},
	}
	_, err = f.model(Config{}).Generate(context.Background(), history, nil)
	require.NoError(t, err)
	require.Contains(t, f.bodies[1], rs+","+c)
}

func TestHistoryWithoutRaw(t *testing.T) {
	f := newServer(t, stubReply())
	history := []durable.Message{
		{Role: durable.RoleUser, Text: "run it"},
		{Role: durable.RoleAssistant, Text: "ok", ToolCalls: []durable.ToolCall{
			{ID: "call_1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)},
			{ID: "call_2", Name: "bash"},
		}},
		{Role: durable.RoleTool, ToolCallID: "call_1", Text: "files"},
		{Role: durable.RoleTool, ToolCallID: "call_2", Text: "boom", IsError: true},
		{Role: durable.RoleUser, Text: "again"},
	}
	_, err := f.model(Config{}).Generate(context.Background(), history, nil)
	require.NoError(t, err)
	require.JSONEq(t, `[
		{"type":"message","role":"user","content":"run it"},
		{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]},
		{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"command\":\"ls\"}"},
		{"type":"function_call","call_id":"call_2","name":"bash","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"files"},
		{"type":"function_call_output","call_id":"call_2","output":"Error: boom"},
		{"type":"message","role":"user","content":"again"}
	]`, mustJSON(t, f.body(t, 0)["input"]))
	require.NotContains(t, f.bodies[0], `"id"`)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func TestEmptyToolResult(t *testing.T) {
	f := newServer(t, stubReply())
	history := []durable.Message{
		{Role: durable.RoleUser, Text: "x"},
		{Role: durable.RoleAssistant, ToolCalls: []durable.ToolCall{{ID: "c", Name: "bash"}}},
		{Role: durable.RoleTool, ToolCallID: "c"},
	}
	_, err := f.model(Config{}).Generate(context.Background(), history, nil)
	require.NoError(t, err)
	require.Contains(t, f.bodies[0], `"output":"(no tool output)"`)
}

func TestRequestBody(t *testing.T) {
	f := newServer(t, stubReply(), stubReply())
	_, err := generate(t, f, Config{System: "be brief", SessionID: "sess-1"})
	require.NoError(t, err)
	b := f.body(t, 0)
	require.Equal(t, false, b["store"])
	require.Equal(t, true, b["stream"])
	require.Equal(t, "be brief", b["instructions"])
	require.Equal(t, "test/model", b["model"])
	require.NotContains(t, b, "reasoning")
	require.NotContains(t, b, "include")
	require.NotContains(t, b, "max_output_tokens")
	require.NotContains(t, b, "previous_response_id")
	require.Equal(t, "sess-1", f.headers[0].Get("x-session-id"))

	_, err = generate(t, f, Config{Effort: "high", MaxTokens: 4})
	require.NoError(t, err)
	b = f.body(t, 1)
	require.Equal(t, false, b["store"])
	require.Equal(t, map[string]any{"effort": "high"}, b["reasoning"])
	require.Equal(t, []any{"reasoning.encrypted_content"}, b["include"])
	require.EqualValues(t, 16, b["max_output_tokens"])
	require.Empty(t, f.headers[1].Get("x-session-id"))
}

func TestMaxTokensPassedThrough(t *testing.T) {
	f := newServer(t, stubReply())
	_, err := generate(t, f, Config{MaxTokens: 2000})
	require.NoError(t, err)
	require.EqualValues(t, 2000, f.body(t, 0)["max_output_tokens"])
}

func TestTools(t *testing.T) {
	f := newServer(t, stubReply())
	specs := []durable.ToolSpec{
		{Name: "a", Description: "da", Schema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`)},
		{Name: "b", Description: "db"},
	}
	_, err := f.model(Config{}).Generate(context.Background(), prompt, specs)
	require.NoError(t, err)
	require.JSONEq(t, `[
		{"type":"function","name":"a","description":"da","parameters":{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}},
		{"type":"function","name":"b","description":"db","parameters":{"type":"object","properties":{}}}
	]`, mustJSON(t, f.body(t, 0)["tools"]))
}

func TestNilAndEmptyInput(t *testing.T) {
	f := newServer(t, stubReply())
	_, err := f.model(Config{}).Generate(context.Background(), nil, nil)
	require.NoError(t, err)
	require.Equal(t, []any{}, f.body(t, 0)["input"])
	require.NotContains(t, f.body(t, 0), "tools")

	_, err = f.model(Config{}).Generate(context.Background(), []durable.Message{}, []durable.ToolSpec{})
	require.NoError(t, err)
}

func TestHTTPStatusClasses(t *testing.T) {
	for _, status := range []int{408, 409, 429, 500, 502, 503} {
		t.Run(fmt.Sprint("transient_", status), func(t *testing.T) {
			_, err := generate(t, newServer(t, reply{status, `{"error":{"message":"x"}}`}), Config{})
			require.Error(t, err)
			require.True(t, isTransient(err))
		})
	}
	for _, status := range []int{400, 401, 402, 403, 404} {
		t.Run(fmt.Sprint("permanent_", status), func(t *testing.T) {
			_, err := generate(t, newServer(t, reply{status, `{"error":{"message":"bad thing"}}`}), Config{})
			require.Error(t, err)
			require.False(t, isTransient(err))
			require.Contains(t, err.Error(), "bad thing")
		})
	}
}

func TestErrorBodyIsLimited(t *testing.T) {
	_, err := generate(t, newServer(t, reply{400, strings.Repeat("x", 1<<20)}), Config{})
	require.Error(t, err)
	require.Less(t, len(err.Error()), 8192)
}

func TestStreamErrors(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		transient bool
		contains  string
	}{
		{"error event transient", fakeapi.ResponsesError("server_error", "boom"), true, "boom"},
		{"error event rate limit", fakeapi.ResponsesError("rate_limit_exceeded", "slow"), true, "slow"},
		{"error event permanent", fakeapi.ResponsesError("invalid_prompt", "nope"), false, "invalid_prompt"},
		{"numeric code retries like HTTP", fakeapi.ResponsesError("524", "upstream timeout"), true, "upstream timeout"},
		{"numeric code unknown 5xx", fakeapi.Event("error", `{"type":"error","code":520,"message":"odd"}`), true, "odd"},
		{"numeric code permanent", fakeapi.ResponsesError("400", "bad"), false, "bad"},
		{"completed with no output", completed(), true, "no output"},
		{"failed transient", fakeapi.ResponsesFailed("server_error", "boom"), true, "boom"},
		{"failed permanent", fakeapi.ResponsesFailed("invalid_prompt", "nope"), false, "nope"},
		{"nested error event", fakeapi.Event("error", `{"type":"error","error":{"code":"overloaded","message":"busy"}}`), true, "busy"},
		{"incomplete max tokens", fakeapi.ResponsesIncomplete("max_output_tokens"), false, "max_output_tokens"},
		{"incomplete content filter", fakeapi.ResponsesIncomplete("content_filter"), false, "content_filter"},
		{"incomplete no reason", fakeapi.ResponsesIncomplete(""), false, "no reason given"},
		{"eof before terminal", fakeapi.ResponsesItem(0, fakeapi.ResponsesMessageItem("m", "par")), true, "terminal"},
		{"empty stream", "", true, "terminal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := generate(t, newServer(t, ok(c.body)), Config{})
			require.Error(t, err)
			require.Equal(t, c.transient, isTransient(err), err.Error())
			require.Contains(t, err.Error(), c.contains)
		})
	}
}

func TestIncompleteNeverRunsPartialToolCall(t *testing.T) {
	c := fakeapi.ResponsesCallItem("fc_1", "call_1", "bash", `{"command":"rm`)
	out, err := generate(t, newServer(t, ok(fakeapi.ResponsesItem(0, c)+fakeapi.ResponsesIncomplete("max_output_tokens"))), Config{})
	require.Error(t, err)
	require.Empty(t, out.ToolCalls)
}

func TestUnfinishedToolCallRejected(t *testing.T) {
	added := fakeapi.ResponsesItemAdded(0, `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"bash","arguments":""}`)
	delta := fakeapi.Event("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"command\":\"rm -rf /tmp/build"}`)
	out, err := generate(t, newServer(t, ok(added+delta+completed())), Config{})
	require.Error(t, err)
	require.False(t, isTransient(err))
	require.Empty(t, out.ToolCalls)
}

func TestMissingOutputIndexRejected(t *testing.T) {
	a := fakeapi.ResponsesCallItem("fc_a", "call_a", "bash", `{"command":"echo a"}`)
	b := fakeapi.ResponsesCallItem("fc_b", "call_b", "bash", `{"command":"echo b"}`)
	ev := func(typ, item string) string {
		return fakeapi.Event(typ, fmt.Sprintf(`{"type":%q,"item":%s}`, typ, item))
	}
	body := ev("response.output_item.added", a) + ev("response.output_item.added", b) +
		ev("response.output_item.done", a) + ev("response.output_item.done", b) + completed()
	out, err := generate(t, newServer(t, ok(body)), Config{})
	require.Error(t, err)
	require.False(t, isTransient(err))
	require.Empty(t, out.ToolCalls)
}

func TestEncryptedContentBackfilled(t *testing.T) {
	streamed := `{"type":"reasoning","id":"rs_1","summary":[]}`
	final := `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc<>&"}`
	msg := fakeapi.ResponsesMessageItem("msg_1", "hi")
	f := newServer(t, ok(fakeapi.ResponsesItem(0, streamed)+fakeapi.ResponsesItem(1, msg)+completed(final, msg)))
	out, err := generate(t, f, Config{})
	require.NoError(t, err)
	var items []map[string]any
	require.NoError(t, json.Unmarshal(out.Raw, &items))
	require.Len(t, items, 2)
	require.Equal(t, "enc<>&", items[0]["encrypted_content"])
	require.Contains(t, string(out.Raw), `"enc<>&"`)
	require.Equal(t, "hi", out.Text)
}

func TestFinalOutputUsedWithoutItems(t *testing.T) {
	msg := fakeapi.ResponsesMessageItem("msg_1", "from final")
	out, err := generate(t, newServer(t, ok(completed(msg))), Config{})
	require.NoError(t, err)
	require.Equal(t, "from final", out.Text)
}

func TestRefusal(t *testing.T) {
	msg := `{"type":"message","id":"m","role":"assistant","content":[{"type":"refusal","refusal":"cannot help"}]}`
	_, err := generate(t, newServer(t, ok(fakeapi.ResponsesItem(0, msg)+completed(msg))), Config{})
	require.ErrorContains(t, err, "cannot help")
	require.False(t, isTransient(err))
}

func TestCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "m"}).Generate(ctx, prompt, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, isTransient(err))
}

func TestNetworkFaultIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, err := New(Config{BaseURL: url, APIKey: "k", Model: "m"}).Generate(context.Background(), prompt, nil)
	require.Error(t, err)
	require.True(t, isTransient(err))
}

func TestAPIKeyNeverInErrors(t *testing.T) {
	const key = "sk-secret-key-123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid key `+r.Header.Get("Authorization")+`"}}`)
	}))
	t.Cleanup(srv.Close)

	_, err := New(Config{BaseURL: srv.URL, APIKey: key, Model: "m"}).Generate(context.Background(), prompt, nil)
	require.Error(t, err)
	require.NotContains(t, err.Error(), key)

	stream := newServer(t, ok(fakeapi.ResponsesError("bad_key", "key "+key+" rejected")))
	_, err = generate(t, stream, Config{APIKey: key})
	require.Error(t, err)
	require.NotContains(t, err.Error(), key)

	// A key in the URL userinfo or path would surface in transport errors.
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	_, err = New(Config{BaseURL: url + "/" + key, APIKey: key, Model: "m"}).Generate(context.Background(), prompt, nil)
	require.Error(t, err)
	require.NotContains(t, err.Error(), key)
}
