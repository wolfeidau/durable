package catalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
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
	forget(t, srv.URL)
	return srv
}

// forget drops the cached list, because a later test's server can reuse the port.
func forget(t *testing.T, url string) {
	t.Helper()
	t.Cleanup(func() {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		delete(cache.lists, url)
	})
}

const haikuEntry = `{"id":"anthropic/claude-haiku-5.5","context_length":1000000,"pricing":{"prompt":"0.0000001","completion":"0.0000005","input_cache_read":"0.00000001","input_cache_write":"0.000000125","overrides":[{"min_prompt_tokens":100000,"prompt":"0.0000005","completion":"0.0000025","input_cache_read":"0.00000005","input_cache_write":"0.000000625"}]}}`

func TestOpenRouterLookup(t *testing.T) {
	const list = `{"data":[{"id":"a/other","context_length":8000},{"id":"~a/alias","context_length":64000},{"id":"a/zero","context_length":0},` +
		`{"id":"a/router","context_length":5,"pricing":{"prompt":"-1","completion":"-1"}},{"id":"a/badprice","context_length":6,"pricing":{"prompt":"lots"}},` +
		`{"id":"a/numeric","context_length":7,"pricing":{"prompt":0.5,"completion":1}},` + haikuEntry + `]}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
		model  string
		want   Info
		errMsg string
	}{
		{"found", 200, list, "a/other", Info{ContextWindow: 8000}, ""},
		{"alias compares exactly", 200, list, "~a/alias", Info{ContextWindow: 64000}, ""},
		{"missing", 200, list, "a/alias", Info{}, "not in the model list"},
		{"bad json", 200, `{`, "a/other", Info{}, "failed to decode"},
		{"empty body", 200, ``, "a/other", Info{}, "failed to decode"},
		{"non-200", 500, list, "a/other", Info{}, "HTTP 500"},
		{"no context length is not an error", 200, list, "a/zero", Info{}, ""},
		{"router placeholder has no price", 200, list, "a/router", Info{ContextWindow: 5}, ""},
		{"malformed price is unknown", 200, list, "a/badprice", Info{ContextWindow: 6}, ""},
		{"numeric price", 200, list, "a/numeric", Info{ContextWindow: 7, Price: &Price{Base: Rates{Prompt: 0.5, Completion: 1}}}, ""},
		{"priced with tiers", 200, list, "anthropic/claude-haiku-5.5", Info{ContextWindow: 1_000_000, Price: &Price{
			Base:  Rates{Prompt: 1e-7, Completion: 5e-7, CacheRead: 1e-8, CacheWrite: 1.25e-7},
			Tiers: []Tier{{MinPromptTokens: 100000, Rates: Rates{Prompt: 5e-7, Completion: 2.5e-6, CacheRead: 5e-8, CacheWrite: 6.25e-7}}},
		}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := modelsServer(t, tc.status, tc.body)
			got, err := OpenRouter{BaseURL: srv.URL, Key: "secret-key", Client: srv.Client()}.Lookup(context.Background(), tc.model)
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

func TestOpenRouterSendsTheKeyOnlyWhenSet(t *testing.T) {
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	t.Cleanup(srv.Close)
	forget(t, srv.URL)

	_, err := OpenRouter{BaseURL: srv.URL}.Lookup(context.Background(), "m")
	require.NoError(t, err)
	require.Empty(t, auth.Load())
}

func TestOpenRouterUnreachableDoesNotLeakKey(t *testing.T) {
	forget(t, "http://127.0.0.1:1")
	_, err := OpenRouter{BaseURL: "http://127.0.0.1:1", Key: "secret-key"}.Lookup(context.Background(), "m")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret-key")
}

func fakeClock(t *testing.T, start time.Time) *time.Time {
	t.Helper()
	now := start
	old := cache.now
	cache.now = func() time.Time { return now }
	t.Cleanup(func() { cache.now = old })
	return &now
}

func TestListCacheExpiresAndRemembersFailuresBriefly(t *testing.T) {
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
	forget(t, srv.URL)

	now := fakeClock(t, time.Unix(1000, 0))
	get := func() (Info, error) {
		return OpenRouter{BaseURL: srv.URL, Client: srv.Client()}.Lookup(context.Background(), "m")
	}

	fail.Store(true)
	_, err := get()
	require.Error(t, err)
	fail.Store(false)
	_, err = get()
	require.Error(t, err, "a failure is remembered briefly")
	require.EqualValues(t, 1, hits.Load())

	*now = now.Add(retryAfter)
	info, err := get()
	require.NoError(t, err)
	require.EqualValues(t, 10, info.ContextWindow)
	require.EqualValues(t, 2, hits.Load(), "retried after retryAfter")

	_, err = get()
	require.NoError(t, err)
	require.EqualValues(t, 2, hits.Load(), "served from the cache")

	*now = now.Add(listTTL - time.Second)
	_, err = get()
	require.NoError(t, err)
	require.EqualValues(t, 2, hits.Load())

	*now = now.Add(2 * time.Second)
	_, err = get()
	require.NoError(t, err)
	require.EqualValues(t, 3, hits.Load(), "refetched after the TTL")
}

func TestConcurrentLookupsShareOneFetch(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-release
		_, _ = w.Write([]byte(`{"data":[{"id":"m","context_length":10}]}`))
	}))
	t.Cleanup(srv.Close)
	forget(t, srv.URL)

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			_, err := OpenRouter{BaseURL: srv.URL, Client: srv.Client()}.Lookup(context.Background(), "m")
			errs <- err
		})
	}
	require.Eventually(t, func() bool { return hits.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, hits.Load())
}

func TestWaiterStopsWaitingWhenItsContextEnds(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	forget(t, srv.URL)

	o := OpenRouter{BaseURL: srv.URL, Client: srv.Client()}
	go func() { _, _ = o.Lookup(context.Background(), "m") }()
	require.Eventually(t, func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return cache.lists[srv.URL] != nil
	}, 5*time.Second, 5*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := o.Lookup(ctx, "m")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestAnthropicDirectMapsIDs(t *testing.T) {
	for in, want := range map[string]string{
		"claude-sonnet-5-5":   "anthropic/claude-sonnet-5.5",
		"claude-haiku-5-5":    "anthropic/claude-haiku-5.5",
		"claude-opus-4-1":     "anthropic/claude-opus-4.1",
		"claude-opus-5":       "anthropic/claude-opus-5",
		"claude-sonnet-5-5.1": "anthropic/claude-sonnet-5-5.1",
		"":                    "anthropic/claude-sonnet-5.5",
	} {
		var got string
		a := AnthropicDirect{Prices: lookupFunc(func(_ context.Context, m string) (Info, error) { got = m; return Info{}, nil }), DefaultModel: "claude-sonnet-5-5"}
		_, err := a.Lookup(context.Background(), in)
		require.NoError(t, err)
		require.Equal(t, want, got, in)
	}
}

type lookupFunc func(context.Context, string) (Info, error)

func (f lookupFunc) Lookup(ctx context.Context, m string) (Info, error) { return f(ctx, m) }

func TestAnthropicDirectUsesTheListWithoutAKey(t *testing.T) {
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"data":[` + haikuEntry + `,{"id":"anthropic/claude-sonnet-5.5","context_length":200000}]}`))
	}))
	t.Cleanup(srv.Close)
	forget(t, srv.URL)

	a := AnthropicDirect{Prices: OpenRouter{BaseURL: srv.URL}, DefaultModel: "claude-sonnet-5-5"}
	info, err := a.Lookup(context.Background(), "")
	require.NoError(t, err)
	require.EqualValues(t, 200000, info.ContextWindow)
	require.Empty(t, auth.Load())

	_, err = a.Lookup(context.Background(), "claude-unlisted-1-1")
	require.ErrorContains(t, err, "not in the model list")
}

func TestEstimate(t *testing.T) {
	price := &Price{
		Base: Rates{Prompt: 1e-7, Completion: 5e-7, CacheRead: 1e-8, CacheWrite: 1.25e-7},
		Tiers: []Tier{
			{MinPromptTokens: 100000, Rates: Rates{Prompt: 5e-7, Completion: 2.5e-6, CacheRead: 5e-8, CacheWrite: 6.25e-7}},
			{MinPromptTokens: 50000, Rates: Rates{Prompt: 3e-7, Completion: 1e-6, CacheRead: 3e-8, CacheWrite: 3e-7}},
		},
	}
	for _, tc := range []struct {
		name  string
		usage durable.Usage
		want  float64
	}{
		{"base tier", durable.Usage{InputTokens: 1000, CacheReadTokens: 2000, CacheWriteTokens: 400, OutputTokens: 100},
			1000*1e-7 + 2000*1e-8 + 400*1.25e-7 + 100*5e-7},
		{"highest reached tier wins", durable.Usage{InputTokens: 10, CacheReadTokens: 120000, OutputTokens: 100},
			10*5e-7 + 120000*5e-8 + 100*2.5e-6},
		{"middle tier counts the whole prompt", durable.Usage{InputTokens: 20000, CacheReadTokens: 30000, OutputTokens: 10},
			20000*3e-7 + 30000*3e-8 + 10*1e-6},
		{"threshold is inclusive", durable.Usage{InputTokens: 100000, OutputTokens: 1}, 100000*5e-7 + 2.5e-6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.InDelta(t, tc.want, price.Estimate(&tc.usage), 1e-12)
		})
	}
}

func TestPendingBecomesReady(t *testing.T) {
	release := make(chan struct{})
	done := make(chan Info, 1)
	p := Start(context.Background(), lookupFunc(func(context.Context, string) (Info, error) {
		<-release
		return Info{ContextWindow: 7}, nil
	}), "m", func(i Info, err error) {
		require.NoError(t, err)
		done <- i
	})

	_, ok := p.Info()
	require.False(t, ok, "not ready before the lookup lands")
	close(release)
	require.EqualValues(t, 7, (<-done).ContextWindow)
	info, ok := p.Info()
	require.True(t, ok)
	require.EqualValues(t, 7, info.ContextWindow)
}

func TestPendingFailureStaysNotReady(t *testing.T) {
	boom := errors.New("boom")
	got := make(chan error, 1)
	p := Start(context.Background(), lookupFunc(func(context.Context, string) (Info, error) {
		return Info{ContextWindow: 7}, boom
	}), "m", func(_ Info, err error) { got <- err })

	require.Equal(t, boom, <-got)
	_, ok := p.Info()
	require.False(t, ok)
}

func TestPendingNilDoneAndParentCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ready := make(chan struct{})
	p := Start(ctx, lookupFunc(func(ctx context.Context, _ string) (Info, error) {
		defer close(ready)
		return Info{ContextWindow: 3}, ctx.Err()
	}), "m", nil)

	<-ready
	require.Eventually(t, func() bool { _, ok := p.Info(); return ok }, 5*time.Second, time.Millisecond, "an ended parent context doesn't cancel the lookup")
}
