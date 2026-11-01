package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/fakeapi"
)

const (
	summaryText   = "CHECKPOINT-SUMMARY"
	oldestMarker  = "OLDEST-MARKER"
	newestMarker  = "NEWEST-MARKER"
	summariserTag = "context summarization assistant"
)

// step is one scripted reply to a main (non-summariser) request.
type step struct {
	call     bool  // a bash call, else text
	usage    int64 // reported input tokens
	hang     bool  // never answers, so a test can kill the agent mid-call
	overflow bool  // HTTP 400 context-length error
}

// compactAPI plays OpenRouter with a script, and answers summariser requests with summaryText.
type compactAPI struct {
	modelsStub
	srv      *httptest.Server
	protocol string
	script   []step

	mu    sync.Mutex
	main  []string
	summ  []string
	paths []string
}

func newCompactAPI(t *testing.T, protocol string, window int64, script ...step) *compactAPI {
	t.Helper()
	f := &compactAPI{protocol: protocol, script: script}
	f.contextLength = window
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *compactAPI) handle(w http.ResponseWriter, r *http.Request) {
	if f.serve(w, r) {
		return
	}
	b, _ := io.ReadAll(r.Body)
	var req struct {
		Instructions string          `json:"instructions"`
		System       json.RawMessage `json:"system"`
	}
	_ = json.Unmarshal(b, &req)

	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	if strings.Contains(req.Instructions+string(req.System), summariserTag) {
		f.summ = append(f.summ, string(b))
		f.mu.Unlock()
		f.reply(w, -1, step{})
		return
	}
	n := len(f.main)
	f.main = append(f.main, string(b))
	st := f.script[min(n, len(f.script)-1)]
	f.mu.Unlock()

	switch {
	case st.hang:
		<-r.Context().Done()
	case st.overflow:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":400,"message":"This model's maximum context length is 8192 tokens. Reduce the length of the messages.","type":"invalid_request_error"}}`)
	default:
		f.reply(w, n, st)
	}
}

func (f *compactAPI) reply(w http.ResponseWriter, n int, st step) {
	w.Header().Set("Content-Type", "text/event-stream")
	text := fmt.Sprintf("reply %d", n)
	if n < 0 {
		text = summaryText
	}
	args, _ := json.Marshal(map[string]any{"command": "true", "timeout_seconds": nil})
	if f.protocol == "/v1/responses" {
		item := fakeapi.ResponsesMessageItem(fmt.Sprintf("msg_%d", n), text)
		if st.call {
			item = fakeapi.ResponsesCallItem(fmt.Sprintf("fc_%d", n), fmt.Sprintf("call_%d", n), "bash", string(args))
		}
		usage := fmt.Sprintf(`{"input_tokens":%d,"output_tokens":1}`, st.usage)
		_, _ = io.WriteString(w, fakeapi.ResponsesItem(0, item)+fakeapi.ResponsesCompletedUsage("["+item+"]", usage))
		return
	}
	block, stop := fakeapi.TextBlock(0, text), "end_turn"
	if st.call {
		block, stop = fakeapi.ToolBlock(0, fmt.Sprintf("toolu_%d", n), "bash", string(args)), "tool_use"
	}
	start := fmt.Sprintf(`{"input_tokens":%d,"output_tokens":1}`, st.usage)
	_, _ = io.WriteString(w, fakeapi.StreamUsage(apiModel, stop, "null", start, `{"output_tokens":5}`, block))
}

func (f *compactAPI) counts() (mainN, summN int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.main), len(f.summ)
}

func (f *compactAPI) requests() (mainBodies, summBodies []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.main...), append([]string(nil), f.summ...)
}

var compactionProtocols = []struct {
	name string
	path string
	args []string
}{
	{"responses", "/v1/responses", []string{"--provider", "openrouter", "--model", routerModel}},
	{"messages", "/v1/messages", []string{"--provider", "openrouter", "--protocol", "anthropic-messages", "--model", routerModel}},
}

func prompt(marker string) string {
	return marker + " " + strings.Repeat("x", 1200) + "\n"
}

func send(t *testing.T, a *agent, s string) {
	t.Helper()
	_, err := io.WriteString(a.stdin, s)
	require.NoError(t, err)
}

func waitCounts(t *testing.T, api *compactAPI, mainN, summN int) {
	t.Helper()
	eventually(t, fmt.Sprintf("%d main and %d summariser requests", mainN, summN), func() bool {
		m, s := api.counts()
		return m == mainN && s == summN
	})
}

// pairedCalls fails unless every tool call in the request has exactly its result, and returns the pair count.
func pairedCalls(t *testing.T, path, body string) int {
	t.Helper()
	calls, results := map[string]bool{}, map[string]bool{}
	if path == "/v1/responses" {
		var req struct {
			Input []struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
			} `json:"input"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &req))
		for _, it := range req.Input {
			switch it.Type {
			case "function_call":
				calls[it.CallID] = true
			case "function_call_output":
				results[it.CallID] = true
			}
		}
	} else {
		var req struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &req))
		for _, m := range req.Messages {
			var blocks []struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				ToolUseID string `json:"tool_use_id"`
			}
			if json.Unmarshal(m.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				switch b.Type {
				case "tool_use":
					calls[b.ID] = true
				case "tool_result":
					results[b.ToolUseID] = true
				}
			}
		}
	}
	require.Equal(t, calls, results, body)
	return len(calls)
}

func TestCompactionOnEachProtocol(t *testing.T) {
	bin := buildBinary(t)
	for _, tc := range compactionProtocols {
		t.Run(tc.name, func(t *testing.T) {
			ws, home := newWorkspace(t)
			// A 1000 token window gives a 500 token threshold, with about 250 tokens kept.
			api := newCompactAPI(t, tc.path, 1000, step{call: true, usage: 900}, step{usage: 900}, step{call: true, usage: 50}, step{usage: 50})
			env := routerEnv(home, api.srv.URL, unusedAnthropic(t))

			a := startRouterAgent(t, bin, ws, env, append([]string{"--yes"}, tc.args...)...)
			send(t, a, prompt(oldestMarker))
			waitCounts(t, api, 2, 0)
			send(t, a, prompt(newestMarker))
			waitCounts(t, api, 4, 1)
			closeAndWait(t, a)

			mainBodies, summBodies := api.requests()
			require.Contains(t, summBodies[0], oldestMarker, "the summariser sees the old turn")
			for _, b := range mainBodies[:2] {
				require.Contains(t, b, oldestMarker)
				require.NotContains(t, b, summaryText)
			}
			for _, b := range mainBodies[2:] {
				require.Contains(t, b, summaryText)
				require.Contains(t, b, newestMarker)
				require.NotContains(t, b, oldestMarker)
			}
			require.Equal(t, 1, pairedCalls(t, tc.path, mainBodies[3]))

			out := a.out.String()
			require.Contains(t, out, "[compaction] context window 1000 tokens")
			require.Contains(t, out, "[compacting history]")
			require.NotContains(t, out, summaryText, "the summary must not stream to the terminal")
		})
	}
}

func TestKillAfterCompactionResumesWithSummary(t *testing.T) {
	bin := buildBinary(t)
	for _, tc := range compactionProtocols {
		t.Run(tc.name, func(t *testing.T) {
			ws, home := newWorkspace(t)
			api := newCompactAPI(t, tc.path, 1000, step{usage: 900}, step{hang: true}, step{usage: 50})
			env := routerEnv(home, api.srv.URL, unusedAnthropic(t))

			a := startRouterAgent(t, bin, ws, env, append([]string{"--yes"}, tc.args...)...)
			send(t, a, prompt(oldestMarker))
			waitCounts(t, api, 1, 0)
			send(t, a, prompt(newestMarker))
			waitCounts(t, api, 2, 1)
			a.kill(t)

			b := startRouterAgent(t, bin, ws, env, "--yes")
			closeAndWait(t, b)

			mainN, summN := api.counts()
			require.Equal(t, 3, mainN)
			require.Equal(t, 1, summN, "the resumed session must not summarise the same state again")
			mainBodies, _ := api.requests()
			for _, body := range mainBodies[1:] {
				require.Contains(t, body, summaryText)
				require.NotContains(t, body, oldestMarker)
			}
		})
	}
}

func TestOverflowForcesCompactionAndRetries(t *testing.T) {
	bin := buildBinary(t)
	for _, tc := range compactionProtocols {
		t.Run(tc.name, func(t *testing.T) {
			ws, home := newWorkspace(t)
			// The window is large, so only the provider's overflow error can trigger compaction.
			api := newCompactAPI(t, tc.path, 0, step{usage: 10}, step{overflow: true}, step{usage: 10})
			env := routerEnv(home, api.srv.URL, unusedAnthropic(t))

			a := startRouterAgent(t, bin, ws, env, append([]string{"--yes"}, tc.args...)...)
			send(t, a, prompt(oldestMarker))
			waitCounts(t, api, 1, 0)
			send(t, a, prompt(newestMarker))
			waitCounts(t, api, 3, 1)
			closeAndWait(t, a)

			mainBodies, _ := api.requests()
			require.NotContains(t, mainBodies[1], summaryText)
			require.Contains(t, mainBodies[2], summaryText)
			require.NotContains(t, mainBodies[2], oldestMarker)
			require.Contains(t, a.out.String(), "[compacting history]")
		})
	}
}

func TestModelsFailureStillStarts(t *testing.T) {
	bin := buildBinary(t)
	for _, tc := range compactionProtocols {
		t.Run(tc.name, func(t *testing.T) {
			ws, home := newWorkspace(t)
			api := newCompactAPI(t, tc.path, 0, step{usage: 10})
			api.fail = true
			env := routerEnv(home, api.srv.URL, unusedAnthropic(t))

			a := startRouterAgent(t, bin, ws, env, append([]string{"--yes"}, tc.args...)...)
			send(t, a, "hello\n")
			waitCounts(t, api, 1, 0)
			closeAndWait(t, a)

			out := a.out.String()
			require.Contains(t, out, "[compaction] warning:")
			require.Contains(t, out, "compaction runs only if the provider reports an overflow")
		})
	}
}

func TestDirectAnthropicHasNoCompaction(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, textReply("hi"))
	}))
	t.Cleanup(srv.Close)

	a := startAgent(t, bin, ws, home, srv.URL)
	send(t, a, "hello\n")
	eventually(t, "the request", func() bool { mu.Lock(); defer mu.Unlock(); return len(paths) == 1 })
	closeAndWait(t, a)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"/v1/messages"}, paths, "no /v1/models request")
	require.NotContains(t, a.out.String(), "[compaction]")
}
