package catalog

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type countingCatalog struct {
	calls atomic.Int32
	fail  atomic.Bool
}

func (c *countingCatalog) Lookup(context.Context, string) (Info, error) {
	c.calls.Add(1)
	if c.fail.Load() {
		return Info{}, errors.New("down")
	}
	return Info{ContextWindow: 42}, nil
}

func TestPendingRetriesAFailedLookup(t *testing.T) {
	now := fakeClock(t, time.Unix(1000, 0))
	c := &countingCatalog{}
	c.fail.Store(true)
	var mu sync.Mutex
	var outcomes []error
	p := Start(context.Background(), c, "m", func(_ Info, err error) {
		mu.Lock()
		defer mu.Unlock()
		outcomes = append(outcomes, err)
	})
	settled := func(n int) {
		t.Helper()
		require.Eventually(t, func() bool {
			p.mu.Lock()
			defer p.mu.Unlock()
			mu.Lock()
			defer mu.Unlock()
			return !p.inFlight && int(c.calls.Load()) == n
		}, 5*time.Second, time.Millisecond)
	}
	settled(1)
	_, ok := p.Info()
	require.False(t, ok)
	settled(1)
	require.Len(t, outcomes, 1)

	// Still failing: a retry runs but stays quiet.
	*now = now.Add(retryAfter)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { p.Info() })
	}
	wg.Wait()
	settled(2)
	mu.Lock()
	require.Len(t, outcomes, 1)
	mu.Unlock()
	_, ok = p.Info()
	require.False(t, ok)
	settled(2)

	// Recovered: the next retry succeeds and reports it.
	c.fail.Store(false)
	*now = now.Add(retryAfter)
	for range 20 {
		wg.Go(func() { p.Info() })
	}
	wg.Wait()
	settled(3)
	info, ok := p.Info()
	require.True(t, ok)
	require.EqualValues(t, 42, info.ContextWindow)
	mu.Lock()
	require.Len(t, outcomes, 2)
	require.NoError(t, outcomes[1])
	mu.Unlock()
	require.EqualValues(t, 3, c.calls.Load())
}
