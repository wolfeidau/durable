package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
)

const (
	liveRouterURL = "https://openrouter.ai/api"
	// Large enough for the session's own prompt and tools to fit under half of it.
	liveWindow   = 24000
	liveCodeword = "ZEPHYR-4417"
	liveEarly    = "EARLYFILLER"
	// A summary may name the filler, so only a verbatim line proves the old message was dropped.
	liveEarlyLine = liveEarly + " line 5 of background notes"
)

var liveCompactionModels = []struct {
	name string
	args []string
}{
	{"responses-glm", []string{"--model", "z-ai/glm-5.3-flash"}},
	{"responses-luna", []string{"--model", "openai/gpt-6-luna", "--effort", "low"}},
	{"messages-glm", []string{"--protocol", "anthropic-messages", "--model", "z-ai/glm-5.3-flash"}},
}

// liveProxy forwards to OpenRouter, and only lowers the window so real summarising happens within a few turns.
type liveProxy struct {
	srv *httptest.Server

	mu     sync.Mutex
	bodies []string
}

func newLiveProxy(t *testing.T) *liveProxy {
	t.Helper()
	target, err := url.Parse(liveRouterURL)
	require.NoError(t, err)
	p := &liveProxy{}
	forward := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.Out.Host = target.Host
	}}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			var list struct {
				Data []map[string]any `json:"data"`
			}
			for _, id := range []string{"z-ai/glm-5.3-flash", "openai/gpt-6-luna"} {
				list.Data = append(list.Data, map[string]any{"id": id, "context_length": liveWindow})
			}
			_ = json.NewEncoder(w).Encode(list)
			return
		}
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		p.mu.Lock()
		p.bodies = append(p.bodies, string(b))
		p.mu.Unlock()
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// mainBodies drops summariser requests, so the assertions see what the session's model was sent.
func (p *liveProxy) mainBodies() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, b := range p.bodies {
		if !strings.Contains(b, summariserTag) {
			out = append(out, b)
		}
	}
	return out
}

func liveFiller(tag string, chars int) string {
	var b strings.Builder
	for i := 0; b.Len() < chars; i++ {
		fmt.Fprintf(&b, "%s line %d of background notes about the harbour survey. ", tag, i)
	}
	return b.String()
}

func requireLiveKey(t *testing.T) string {
	t.Helper()
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" || os.Getenv("DURABLE_SMOKE") != "1" {
		t.Skip("set OPENROUTER_API_KEY and DURABLE_SMOKE=1 to run against the real API")
	}
	return key
}

// Real summaries and real usage figures, then a crash, against the real service.
func TestOpenRouterCompactionLive(t *testing.T) {
	key := requireLiveKey(t)
	bin := buildBinary(t)
	for _, tc := range liveCompactionModels {
		t.Run(tc.name, func(t *testing.T) {
			ws, home := newWorkspace(t)
			proxy := newLiveProxy(t)
			env := append(liveEnv(home), "OPENROUTER_BASE_URL="+proxy.srv.URL)

			a := startRouterAgent(t, bin, ws, env, append([]string{"--yes", "--provider", "openrouter"}, tc.args...)...)
			turns := []struct{ prompt, ack string }{
				{"The project codeword is " + liveCodeword + ". Remember it. Reply with only ACK-ONE. Notes: " + liveFiller(liveEarly, 24000), "ACK-ONE"},
				{"Use bash to run `echo tool-ran`, then reply with only ACK-TWO. Notes: " + liveFiller("MIDFILLER", 16000), "ACK-TWO"},
				{"Reply with only ACK-THREE. Notes: " + liveFiller("LATEFILLER", 16000), "ACK-THREE"},
			}
			for _, turn := range turns {
				send(t, a, turn.prompt+"\n")
				waitLive(t, a, turn.ack, func() bool { return strings.Contains(a.out.String(), turn.ack) })
			}
			send(t, a, "Reply with only ACK-FOUR.\n")
			waitLive(t, a, "compaction", func() bool { return strings.Contains(a.out.String(), "[compacting history]") })
			waitLive(t, a, "ACK-FOUR", func() bool { return strings.Contains(a.out.String(), "ACK-FOUR") })
			a.kill(t)

			b := startRouterAgent(t, bin, ws, env, "--yes")
			send(t, b, "What was the project codeword? Reply with only the codeword.\n")
			waitLive(t, b, "the codeword", func() bool { return strings.Contains(b.out.String(), liveCodeword) })
			closeAndWait(t, b)

			out := a.out.String() + b.out.String()
			require.NotContains(t, out, key)
			require.NotContains(t, out, "[compaction] warning")

			bodies := proxy.mainBodies()
			last := bodies[len(bodies)-1]
			require.Contains(t, last, durable.CompactionSummaryPrefix[:30])
			require.NotContains(t, last, liveEarlyLine)

			var measured int64
			hist := finalHistory(t, ws)
			for _, m := range hist {
				measured = max(measured, m.ContextTokens)
			}
			require.Positive(t, measured, "real usage must reach ContextTokens")
			require.Contains(t, hist[0].Text, liveEarly, "compaction must not rewrite stored messages")
			t.Logf("requests=%d largest measured context=%d", len(bodies), measured)
		})
	}
}

// The overflow patterns are text matching, so this checks them against what providers really send.
func TestOpenRouterOverflowTextLive(t *testing.T) {
	key := requireLiveKey(t)
	bin := buildBinary(t)
	for _, model := range []string{"z-ai/glm-5.3-flash", "openai/gpt-6-luna"} {
		t.Run(model, func(t *testing.T) {
			window := liveContextLength(t, key, model)
			ws, home := newWorkspace(t)
			a := startRouterAgent(t, bin, ws, liveEnv(home), "--yes", "--provider", "openrouter", "--model", model)
			send(t, a, "Reply with OK. "+liveFiller("OVERFLOW", int(window)*6)+"\n")
			waitLiveFailure(t, a)

			out := a.out.String()
			require.NotContains(t, out, key)
			_, msg, _ := strings.Cut(out, "failed: model failed:")
			msg = msg[:min(len(msg), 400)]
			t.Logf("window=%d provider said: %s", window, msg)
			require.True(t, durable.IsOverflowText(msg), "not recognised as overflow: %s", msg)
		})
	}
}

func waitLiveFailure(t *testing.T, a *agent) {
	t.Helper()
	deadline := time.Now().Add(liveTimeout)
	for !strings.Contains(a.out.String(), "failed: model failed:") {
		if time.Now().After(deadline) {
			t.Fatal("waiting for the model to reject the prompt")
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(time.Second) // the rest of the message may still be flushing
}

func liveContextLength(t *testing.T, key, model string) int64 {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, liveRouterURL+"/v1/models", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int64  `json:"context_length"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	for _, m := range list.Data {
		if m.ID == model {
			return m.ContextLength
		}
	}
	t.Fatalf("model %s is not listed", model)
	return 0
}
