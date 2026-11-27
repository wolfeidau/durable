package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/sandbox"
)

type fakeClock struct {
	mu  sync.Mutex
	cur time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{cur: start}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.cur = c.cur.Add(d)
	c.mu.Unlock()
}

type fetchResult struct {
	phases map[string]sandbox.Phase
	err    error
}

type controlledFetcher struct {
	started chan context.Context
	results chan fetchResult
	calls   atomic.Int64
}

func newControlledFetcher() *controlledFetcher {
	return &controlledFetcher{
		started: make(chan context.Context, 16),
		results: make(chan fetchResult, 16),
	}
}

func (f *controlledFetcher) fetch(ctx context.Context) (map[string]sandbox.Phase, error) {
	f.calls.Add(1)
	f.started <- ctx
	select {
	case res := <-f.results:
		return res.phases, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	require.Eventually(t, fn, 5*time.Second, 5*time.Millisecond)
}

func TestSandboxCacheColdStartCoalescingAndFresh(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	f := newControlledFetcher()
	draining := make(chan struct{})
	t.Cleanup(func() { close(draining) })

	c := newSandboxCache(sandboxCacheConfig{
		fetch:        f.fetch,
		ttl:          15 * time.Second,
		retryBackoff: 5 * time.Second,
		timeout:      5 * time.Second,
		draining:     draining,
		now:          clk.now,
	})

	// Cold start returns immediately in ColdRefreshing without blocking on fetch.
	snap := c.Snapshot(false)
	require.True(t, snap.observedAt.IsZero())
	require.True(t, snap.refreshing)
	require.True(t, snap.stale)
	require.NoError(t, snap.err)
	require.Empty(t, snap.phases)

	<-f.started
	require.EqualValues(t, 1, f.calls.Load())

	// Concurrent callers coalesce into the single in-flight refresh.
	snap2 := c.Snapshot(false)
	require.True(t, snap2.refreshing)
	require.True(t, snap2.stale)
	require.True(t, snap2.observedAt.IsZero())
	require.EqualValues(t, 1, f.calls.Load())

	// Complete the fetch and settle into Fresh.
	clk.advance(time.Second)
	f.results <- fetchResult{phases: map[string]sandbox.Phase{"ds-1": sandbox.PhaseRunning}}
	waitFor(t, func() bool { return !c.Snapshot(false).refreshing })

	fresh := c.Snapshot(false)
	require.Equal(t, clk.now(), fresh.observedAt)
	require.False(t, fresh.refreshing)
	require.False(t, fresh.stale)
	require.NoError(t, fresh.err)
	require.Equal(t, map[string]sandbox.Phase{"ds-1": sandbox.PhaseRunning}, fresh.phases)
	require.EqualValues(t, 1, f.calls.Load())

	// Mutating the returned map does not mutate the cache.
	fresh.phases["ds-1"] = sandbox.PhaseError
	require.Equal(t, sandbox.PhaseRunning, c.Snapshot(false).phases["ds-1"])

	// Still Fresh just before TTL.
	clk.advance(14 * time.Second)
	stillFresh := c.Snapshot(false)
	require.False(t, stillFresh.stale)
	require.False(t, stillFresh.refreshing)
	require.EqualValues(t, 1, f.calls.Load())
}

func TestSandboxCacheTTLExpiryAndForceRefresh(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	f := newControlledFetcher()
	draining := make(chan struct{})
	t.Cleanup(func() { close(draining) })

	c := newSandboxCache(sandboxCacheConfig{
		fetch:        f.fetch,
		ttl:          15 * time.Second,
		retryBackoff: 5 * time.Second,
		draining:     draining,
		now:          clk.now,
	})

	_ = c.Snapshot(false)
	<-f.started
	f.results <- fetchResult{phases: map[string]sandbox.Phase{"ds-1": sandbox.PhaseRunning}}
	waitFor(t, func() bool { return !c.Snapshot(false).refreshing })
	firstObs := c.Snapshot(false).observedAt

	// Force refresh while Fresh transitions to StaleRefreshing immediately.
	clk.advance(time.Second)
	forced := c.Snapshot(true)
	require.Equal(t, firstObs, forced.observedAt)
	require.True(t, forced.refreshing)
	require.True(t, forced.stale)
	require.NoError(t, forced.err)
	require.Equal(t, sandbox.PhaseRunning, forced.phases["ds-1"])

	<-f.started
	require.EqualValues(t, 2, f.calls.Load())
	f.results <- fetchResult{phases: map[string]sandbox.Phase{"ds-1": sandbox.PhaseStopped}}
	waitFor(t, func() bool { return !c.Snapshot(false).refreshing })

	afterForce := c.Snapshot(false)
	require.False(t, afterForce.stale)
	require.Equal(t, sandbox.PhaseStopped, afterForce.phases["ds-1"])

	// Advance past TTL: Snapshot(false) transitions Fresh -> StaleRefreshing.
	clk.advance(15 * time.Second)
	expired := c.Snapshot(false)
	require.True(t, expired.stale)
	require.True(t, expired.refreshing)
	require.Equal(t, sandbox.PhaseStopped, expired.phases["ds-1"])

	<-f.started
	require.EqualValues(t, 3, f.calls.Load())
	f.results <- fetchResult{phases: map[string]sandbox.Phase{"ds-1": sandbox.PhaseRunning}}
	waitFor(t, func() bool { return !c.Snapshot(false).refreshing })

	settled := c.Snapshot(false)
	require.False(t, settled.stale)
	require.False(t, settled.refreshing)
	require.Equal(t, sandbox.PhaseRunning, settled.phases["ds-1"])
}

func TestSandboxCacheColdFailureRetryBackoffAndRecovery(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	f := newControlledFetcher()
	draining := make(chan struct{})
	t.Cleanup(func() { close(draining) })
	log := &lockedBuffer{}

	c := newSandboxCache(sandboxCacheConfig{
		fetch:        f.fetch,
		ttl:          15 * time.Second,
		retryBackoff: 5 * time.Second,
		draining:     draining,
		logger:       slog.New(slog.NewTextHandler(log, nil)),
		now:          clk.now,
	})

	_ = c.Snapshot(false)
	<-f.started
	errBoom := errors.New("gateway 10.0.0.1 unreachable")
	f.results <- fetchResult{err: errBoom}
	waitFor(t, func() bool { return !c.Snapshot(false).refreshing })

	// ColdFailed before retryBackoff does not start a second fetch.
	clk.advance(4 * time.Second)
	failed := c.Snapshot(false)
	require.True(t, failed.observedAt.IsZero())
	require.False(t, failed.refreshing)
	require.True(t, failed.stale)
	require.ErrorIs(t, failed.err, errBoom)
	require.EqualValues(t, 1, f.calls.Load())
	require.Contains(t, log.String(), "gateway 10.0.0.1 unreachable")

	// After retryBackoff, ColdFailed -> ColdRefreshing preserves prior error while retrying.
	clk.advance(time.Second)
	retrying := c.Snapshot(false)
	require.True(t, retrying.observedAt.IsZero())
	require.True(t, retrying.refreshing)
	require.True(t, retrying.stale)
	require.ErrorIs(t, retrying.err, errBoom)

	<-f.started
	require.EqualValues(t, 2, f.calls.Load())
	f.results <- fetchResult{phases: map[string]sandbox.Phase{"ds-1": sandbox.PhaseRunning}}
	waitFor(t, func() bool { return !c.Snapshot(false).refreshing })

	recovered := c.Snapshot(false)
	require.False(t, recovered.observedAt.IsZero())
	require.False(t, recovered.refreshing)
	require.False(t, recovered.stale)
	require.NoError(t, recovered.err)
	require.Equal(t, sandbox.PhaseRunning, recovered.phases["ds-1"])
}

func TestSandboxCacheStaleFailurePreservesCachedPhasesAndErrorAcrossRetries(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	f := newControlledFetcher()
	draining := make(chan struct{})
	t.Cleanup(func() { close(draining) })
	log := &lockedBuffer{}

	c := newSandboxCache(sandboxCacheConfig{
		fetch:        f.fetch,
		ttl:          15 * time.Second,
		retryBackoff: 5 * time.Second,
		draining:     draining,
		logger:       slog.New(slog.NewTextHandler(log, nil)),
		now:          clk.now,
	})

	_ = c.Snapshot(false)
	<-f.started
	f.results <- fetchResult{phases: map[string]sandbox.Phase{"ds-1": sandbox.PhaseRunning}}
	waitFor(t, func() bool { return !c.Snapshot(false).refreshing })
	firstObs := c.Snapshot(false).observedAt

	// Expire TTL and fail the refresh.
	clk.advance(15 * time.Second)
	_ = c.Snapshot(false)
	<-f.started
	errDown := errors.New("gateway timeout")
	f.results <- fetchResult{err: errDown}
	waitFor(t, func() bool { return !c.Snapshot(false).refreshing })

	// StaleFailed preserves previous phases and observedAt.
	clk.advance(2 * time.Second)
	staleFailed := c.Snapshot(false)
	require.Equal(t, firstObs, staleFailed.observedAt)
	require.Equal(t, sandbox.PhaseRunning, staleFailed.phases["ds-1"])
	require.False(t, staleFailed.refreshing)
	require.True(t, staleFailed.stale)
	require.ErrorIs(t, staleFailed.err, errDown)
	require.EqualValues(t, 2, f.calls.Load())

	// forceRefresh bypasses retryBackoff and keeps prior err until fetch succeeds.
	retryForced := c.Snapshot(true)
	require.Equal(t, firstObs, retryForced.observedAt)
	require.Equal(t, sandbox.PhaseRunning, retryForced.phases["ds-1"])
	require.True(t, retryForced.refreshing)
	require.True(t, retryForced.stale)
	require.ErrorIs(t, retryForced.err, errDown)

	<-f.started
	require.EqualValues(t, 3, f.calls.Load())
	f.results <- fetchResult{phases: map[string]sandbox.Phase{"ds-1": sandbox.PhaseStopped}}
	waitFor(t, func() bool { return !c.Snapshot(false).refreshing })

	fresh := c.Snapshot(false)
	require.True(t, fresh.observedAt.After(firstObs))
	require.False(t, fresh.refreshing)
	require.False(t, fresh.stale)
	require.NoError(t, fresh.err)
	require.Equal(t, sandbox.PhaseStopped, fresh.phases["ds-1"])
}

func TestSandboxCacheInvalidate(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	f := newControlledFetcher()
	draining := make(chan struct{})
	t.Cleanup(func() { close(draining) })

	c := newSandboxCache(sandboxCacheConfig{
		fetch:        f.fetch,
		ttl:          15 * time.Second,
		retryBackoff: 5 * time.Second,
		draining:     draining,
		now:          clk.now,
	})

	// Before any query, Invalidate does not touch the gateway.
	c.Invalidate()
	require.EqualValues(t, 0, f.calls.Load())

	// First query kicks a refresh.
	_ = c.Snapshot(false)
	<-f.started
	f.results <- fetchResult{phases: map[string]sandbox.Phase{"ds-1": sandbox.PhaseRunning}}
	waitFor(t, func() bool { return !c.Snapshot(false).refreshing })
	require.EqualValues(t, 1, f.calls.Load())

	// Invalidate after being queried immediately triggers a background refresh even within retryBackoff.
	clk.advance(time.Second)
	c.Invalidate()
	<-f.started
	require.EqualValues(t, 2, f.calls.Load())
	snap := c.Snapshot(false)
	require.True(t, snap.refreshing)
	require.True(t, snap.stale)

	// Invalidate again while refresh #2 is in flight; finishing #2 triggers follow-up #3.
	c.Invalidate()
	f.results <- fetchResult{phases: map[string]sandbox.Phase{"ds-1": sandbox.PhaseStopping}}
	<-f.started
	require.EqualValues(t, 3, f.calls.Load())
	f.results <- fetchResult{phases: map[string]sandbox.Phase{"ds-1": sandbox.PhaseStopped}}
	waitFor(t, func() bool { return !c.Snapshot(false).refreshing })

	final := c.Snapshot(false)
	require.False(t, final.stale)
	require.False(t, final.refreshing)
	require.Equal(t, sandbox.PhaseStopped, final.phases["ds-1"])
}

func TestSandboxCacheDrainingCancelsInFlightRefresh(t *testing.T) {
	t.Parallel()
	f := newControlledFetcher()
	draining := make(chan struct{})

	c := newSandboxCache(sandboxCacheConfig{
		fetch:    f.fetch,
		timeout:  30 * time.Second,
		draining: draining,
	})

	snap := c.Snapshot(false)
	require.True(t, snap.refreshing)
	ctx := <-f.started

	close(draining)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight refresh context was not cancelled on drain")
	}
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	waitFor(t, func() bool { return !c.Snapshot(false).refreshing })

	// After drain, neither Snapshot(true) nor Invalidate launches new fetches.
	_ = c.Snapshot(true)
	c.Invalidate()
	require.EqualValues(t, 1, f.calls.Load())
}

func TestSandboxCacheFetchTimeout(t *testing.T) {
	t.Parallel()
	f := newControlledFetcher()
	draining := make(chan struct{})
	t.Cleanup(func() { close(draining) })

	c := newSandboxCache(sandboxCacheConfig{
		fetch:    f.fetch,
		timeout:  30 * time.Millisecond,
		draining: draining,
	})

	_ = c.Snapshot(false)
	ctx := <-f.started
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("fetch context did not time out")
	}
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	waitFor(t, func() bool { return !c.Snapshot(false).refreshing })
	failed := c.Snapshot(false)
	require.ErrorIs(t, failed.err, context.DeadlineExceeded)
	require.True(t, failed.stale)
}

func TestSandboxCacheConcurrentAccess(t *testing.T) {
	t.Parallel()
	var n atomic.Int64
	draining := make(chan struct{})
	c := newSandboxCache(sandboxCacheConfig{
		fetch: func(ctx context.Context) (map[string]sandbox.Phase, error) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			k := n.Add(1)
			if k%3 == 0 {
				return nil, errors.New("transient error")
			}
			return map[string]sandbox.Phase{"ds-1": sandbox.PhaseRunning}, nil
		},
		ttl:          time.Millisecond,
		retryBackoff: time.Millisecond,
		timeout:      time.Second,
		draining:     draining,
	})

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for i := range 50 {
				_ = c.Snapshot(i%5 == 0)
				if i%7 == 0 {
					c.Invalidate()
				}
			}
		})
	}
	wg.Wait()
	close(draining)
	waitFor(t, func() bool { return !c.Snapshot(false).refreshing })
}
