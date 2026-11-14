package commands

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/claude"
)

func modelsServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	forgetModels(t, srv.URL)
	return srv
}

// forgetModels drops the cached list, because a later test's server can reuse the port.
func forgetModels(t *testing.T, url string) {
	t.Helper()
	t.Cleanup(func() {
		modelCache.mu.Lock()
		defer modelCache.mu.Unlock()
		delete(modelCache.lists, url)
	})
}

const haikuEntry = `{"id":"anthropic/claude-haiku-5.5","context_length":1000000,"pricing":{"prompt":"0.0000001","completion":"0.0000005","input_cache_read":"0.00000001","input_cache_write":"0.000000125","input_cache_write_1h":"0.0000002","overrides":[{"min_prompt_tokens":100000,"prompt":"0.0000005","completion":"0.0000025","input_cache_read":"0.00000005","input_cache_write":"0.000000625","input_cache_write_1h":"0.000001"}]}}`

func TestFetchModelInfo(t *testing.T) {
	const list = `{"data":[{"id":"a/other","context_length":8000},{"id":"~a/alias","context_length":64000},{"id":"a/zero","context_length":0},` +
		`{"id":"a/router","context_length":5,"pricing":{"prompt":"-1","completion":"-1"}},{"id":"a/badprice","context_length":6,"pricing":{"prompt":"lots"}},` + haikuEntry + `]}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
		model  string
		want   modelInfo
		errMsg string
	}{
		{"found", 200, list, "a/other", modelInfo{ContextWindow: 8000}, ""},
		{"alias compares exactly", 200, list, "~a/alias", modelInfo{ContextWindow: 64000}, ""},
		{"missing", 200, list, "a/alias", modelInfo{}, "not in the model list"},
		{"bad json", 200, `{`, "a/other", modelInfo{}, "failed to decode"},
		{"empty body", 200, ``, "a/other", modelInfo{}, "failed to decode"},
		{"non-200", 500, list, "a/other", modelInfo{}, "HTTP 500"},
		{"no context length is not an error", 200, list, "a/zero", modelInfo{}, ""},
		{"router placeholder has no price", 200, list, "a/router", modelInfo{ContextWindow: 5}, ""},
		{"malformed price is unknown", 200, list, "a/badprice", modelInfo{ContextWindow: 6}, ""},
		{"priced with tiers", 200, list, "anthropic/claude-haiku-5.5", modelInfo{ContextWindow: 1_000_000, Price: &modelPrice{
			Base:      rates{Prompt: 1e-7, Completion: 5e-7, CacheRead: 1e-8, CacheWrite: 1.25e-7, CacheWrite1h: 2e-7},
			Overrides: []priceTier{{MinPromptTokens: 100000, Rates: rates{Prompt: 5e-7, Completion: 2.5e-6, CacheRead: 5e-8, CacheWrite: 6.25e-7, CacheWrite1h: 1e-6}}},
		}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := modelsServer(t, tc.status, tc.body)
			got, err := fetchModelInfo(context.Background(), srv.Client(), srv.URL, "secret-key", tc.model)
			if tc.errMsg != "" {
				require.ErrorContains(t, err, tc.errMsg)
				require.NotContains(t, err.Error(), "secret-key")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestFetchModelInfoUnreachableDoesNotLeakKey(t *testing.T) {
	_, err := fetchModelInfo(context.Background(), &http.Client{}, "http://127.0.0.1:1", "secret-key", "m")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret-key")
}

func TestModelListCacheExpiresAndSkipsErrors(t *testing.T) {
	var hits atomic.Int32
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m","context_length":10}]}`))
	}))
	t.Cleanup(srv.Close)
	forgetModels(t, srv.URL)

	now := time.Unix(1000, 0)
	old := modelCache.now
	modelCache.now = func() time.Time { return now }
	t.Cleanup(func() { modelCache.now = old })
	get := func() (modelInfo, error) {
		return fetchModelInfo(context.Background(), srv.Client(), srv.URL, "", "m")
	}

	fail.Store(true)
	_, err := get()
	require.Error(t, err)
	fail.Store(false)
	info, err := get()
	require.NoError(t, err)
	require.EqualValues(t, 10, info.ContextWindow)
	require.EqualValues(t, 2, hits.Load(), "an error is not cached")

	_, err = get()
	require.NoError(t, err)
	require.EqualValues(t, 2, hits.Load(), "served from the cache")

	now = now.Add(modelsTTL - time.Second)
	_, err = get()
	require.NoError(t, err)
	require.EqualValues(t, 2, hits.Load())

	now = now.Add(2 * time.Second)
	_, err = get()
	require.NoError(t, err)
	require.EqualValues(t, 3, hits.Load(), "refetched after the TTL")
}

func TestOpenRouterAnthropicID(t *testing.T) {
	for in, want := range map[string]string{
		"claude-sonnet-5-5":   "anthropic/claude-sonnet-5.5",
		"claude-haiku-5-5":    "anthropic/claude-haiku-5.5",
		"claude-opus-4-1":     "anthropic/claude-opus-4.1",
		"claude-opus-5":       "anthropic/claude-opus-5",
		"claude-sonnet-5-5.1": "anthropic/claude-sonnet-5-5.1",
	} {
		require.Equal(t, want, openRouterAnthropicID(in), in)
	}
	require.Equal(t, "anthropic/claude-sonnet-5.5", openRouterAnthropicID(claude.DefaultModel))
}

func TestLoadModelInfoDirectAnthropicUsesTheOpenRouterListWithoutAKey(t *testing.T) {
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"data":[` + haikuEntry + `,{"id":"anthropic/claude-sonnet-5.5","context_length":200000}]}`))
	}))
	t.Cleanup(srv.Close)
	forgetModels(t, srv.URL)
	t.Setenv("OPENROUTER_BASE_URL", srv.URL)

	rt := route{Provider: providerAnthropic, Protocol: protocolMessages, Model: "claude-sonnet-5-5"}
	info, err := loadModelInfo(context.Background(), rt, providers[providerAnthropic], "anthropic-secret")
	require.NoError(t, err)
	require.EqualValues(t, 200000, info.ContextWindow)
	require.Nil(t, info.Price)
	require.Empty(t, auth.Load())

	rt.Model = ""
	info, err = loadModelInfo(context.Background(), rt, providers[providerAnthropic], "")
	require.NoError(t, err)
	require.EqualValues(t, 200000, info.ContextWindow, "the default model")

	rt.Model = "claude-unlisted-1-1"
	_, err = loadModelInfo(context.Background(), rt, providers[providerAnthropic], "")
	require.ErrorContains(t, err, "not in the model list")
}
