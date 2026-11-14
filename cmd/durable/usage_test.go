package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/fakeapi"
)

const usageModel = "anthropic/claude-haiku-5.5"

// usageAPI plays OpenRouter with a priced model list and a reply that reports token usage.
func usageAPI(t *testing.T, path, cost string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch r.URL.Path {
		case "/v1/models":
			_, _ = fmt.Fprintf(w, `{"data":[{"id":%q,"context_length":1000000,"pricing":{"prompt":"0.000001","completion":"0.000002","input_cache_read":"0.0000001","input_cache_write":"0.0000005"}}]}`, usageModel)
		case path:
			// A little latency so the recorded duration is above zero.
			time.Sleep(5 * time.Millisecond)
			w.Header().Set("Content-Type", "text/event-stream")
			if path == "/v1/responses" {
				item := fakeapi.ResponsesMessageItem("msg_1", "hello")
				usage := `{"input_tokens":1000,"input_tokens_details":{"cached_tokens":400},"output_tokens":50,"output_tokens_details":{"reasoning_tokens":10}` + cost + `}`
				_, _ = io.WriteString(w, fakeapi.ResponsesItemAdded(0, `{"type":"message","id":"msg_1","role":"assistant","content":[]}`)+fakeapi.ResponsesTextDelta(0, "hello")+fakeapi.ResponsesItemDone(0, item)+fakeapi.ResponsesCompletedUsage("["+item+"]", usage))
				return
			}
			_, _ = io.WriteString(w, fakeapi.StreamUsage(usageModel, "end_turn", "null",
				`{"input_tokens":600,"cache_read_input_tokens":400,"output_tokens":1}`, `{"output_tokens":50`+cost+`}`, fakeapi.TextBlock(0, "hello")))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func storedReply(t *testing.T, ws string) durable.Message {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(sessionDir(t, ws), "session.sqlite"))
	require.NoError(t, err)
	defer db.Close()
	var body string
	require.NoError(t, db.QueryRow(`SELECT body FROM messages WHERE json_extract(body, '$.role') = 'assistant' ORDER BY seq LIMIT 1`).Scan(&body))
	var m durable.Message
	require.NoError(t, json.Unmarshal([]byte(body), &m))
	return m
}

func TestReplyUsageIsStored(t *testing.T) {
	for _, tc := range []struct {
		name       string
		path       string
		args       []string
		cost       string
		wantSource string
		wantCost   float64
		wantRead   int64
		wantInput  int64
	}{
		// 600 uncached input x 1e-6 + 400 cache read x 1e-7 + 50 output x 2e-6.
		{"messages estimate", "/v1/messages", []string{"--protocol", "anthropic-messages"}, "", durable.CostEstimate, 0.00074, 400, 600},
		{"responses provider cost", "/v1/responses", nil, `,"cost":0.5`, durable.CostProvider, 0.5, 400, 600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, home := newWorkspace(t)
			srv := usageAPI(t, tc.path, tc.cost)
			env := routerEnv(home, srv.URL, unusedAnthropic(t))

			a := startRouterAgent(t, binPath, ws, env, append([]string{"--yes", "--provider", "openrouter", "--model", usageModel}, tc.args...)...)
			_, err := io.WriteString(a.stdin, "hi\n")
			require.NoError(t, err)
			eventually(t, "the reply", func() bool { return strings.Contains(a.out.String(), "hello") })
			closeAndWait(t, a)

			u := storedReply(t, ws).Usage
			require.NotNil(t, u)
			require.Equal(t, tc.wantInput, u.InputTokens)
			require.Equal(t, tc.wantRead, u.CacheReadTokens)
			require.EqualValues(t, 50, u.OutputTokens)
			require.Equal(t, tc.wantSource, u.CostSource)
			require.InDelta(t, tc.wantCost, u.CostUSD, 1e-9)
			require.Equal(t, usageModel, u.Model)
			require.EqualValues(t, 1_000_000, u.ContextWindow)
			require.Positive(t, u.DurationMs)
		})
	}
}
