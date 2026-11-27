package server

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/wolfeidau/durable/internal/sandbox"
)

const (
	defaultSandboxCacheTTL          = 15 * time.Second
	defaultSandboxCacheRetryBackoff = 5 * time.Second
	defaultSandboxCacheTimeout      = 5 * time.Second
)

type sandboxSnapshot struct {
	phases     map[string]sandbox.Phase
	observedAt time.Time
	refreshing bool
	stale      bool
	err        error
}

type sandboxCacheConfig struct {
	fetch        func(context.Context) (map[string]sandbox.Phase, error)
	ttl          time.Duration
	retryBackoff time.Duration
	timeout      time.Duration
	draining     <-chan struct{}
	logger       *slog.Logger
	now          func() time.Time
}

// sandboxCache keeps an eventually-consistent snapshot of sandbox phases without blocking ListSessions.
type sandboxCache struct {
	fetch        func(context.Context) (map[string]sandbox.Phase, error)
	ttl          time.Duration
	retryBackoff time.Duration
	timeout      time.Duration
	draining     <-chan struct{}
	logger       *slog.Logger
	now          func() time.Time

	mu          sync.Mutex
	queried     bool
	phases      map[string]sandbox.Phase
	observedAt  time.Time
	lastAttempt time.Time
	refreshing  bool
	dirty       bool
	gen         uint64
	err         error
}

func newSandboxCache(cfg sandboxCacheConfig) *sandboxCache {
	if cfg.ttl <= 0 {
		cfg.ttl = defaultSandboxCacheTTL
	}
	if cfg.retryBackoff <= 0 {
		cfg.retryBackoff = defaultSandboxCacheRetryBackoff
	}
	if cfg.timeout <= 0 {
		cfg.timeout = defaultSandboxCacheTimeout
	}
	if cfg.logger == nil {
		cfg.logger = slog.New(slog.DiscardHandler)
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	return &sandboxCache{
		fetch:        cfg.fetch,
		ttl:          cfg.ttl,
		retryBackoff: cfg.retryBackoff,
		timeout:      cfg.timeout,
		draining:     cfg.draining,
		logger:       cfg.logger,
		now:          cfg.now,
	}
}

// Snapshot returns the cached snapshot immediately and kicks at most one background refresh when needed.
func (c *sandboxCache) Snapshot(forceRefresh bool) sandboxSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.queried = true
	now := c.now()
	draining := c.isDraining()
	if forceRefresh && !draining {
		c.dirty = true
	}
	stale := c.isStaleLocked(now)
	shouldRefresh := !c.refreshing && !draining && (forceRefresh || (stale && (c.lastAttempt.IsZero() || now.Sub(c.lastAttempt) >= c.retryBackoff)))
	if shouldRefresh {
		c.startRefreshLocked()
	}
	return sandboxSnapshot{
		phases:     maps.Clone(c.phases),
		observedAt: c.observedAt,
		refreshing: c.refreshing,
		stale:      stale,
		err:        c.err,
	}
}

// Invalidate marks the snapshot stale and refreshes in the background once the cache has been queried.
func (c *sandboxCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.gen++
	c.dirty = true
	if c.queried && !c.refreshing && !c.isDraining() {
		c.startRefreshLocked()
	}
}

func (c *sandboxCache) isStaleLocked(now time.Time) bool {
	return c.observedAt.IsZero() || c.dirty || c.err != nil || now.Sub(c.observedAt) >= c.ttl
}

func (c *sandboxCache) isDraining() bool {
	if c.draining == nil {
		return false
	}
	select {
	case <-c.draining:
		return true
	default:
		return false
	}
}

func (c *sandboxCache) startRefreshLocked() {
	c.refreshing = true
	gen := c.gen
	go c.runRefresh(gen)
}

func (c *sandboxCache) runRefresh(startedGen uint64) {
	ctx, cancel := c.refreshContext()
	defer cancel()

	phases, err := c.fetch(ctx)
	if err != nil {
		if !c.isDraining() {
			c.logger.WarnContext(ctx, "sandbox status refresh failed", slog.Any("error", err))
		}
		c.mu.Lock()
		c.lastAttempt = c.now()
		c.err = err
		c.refreshing = false
		c.mu.Unlock()
		return
	}

	c.mu.Lock()
	now := c.now()
	c.lastAttempt = now
	c.phases = phases
	c.observedAt = now
	c.err = nil
	if c.gen != startedGen && c.queried && !c.isDraining() {
		// Invalidate ran while this fetch was in flight; fetch once more so local changes are not missed.
		c.dirty = true
		c.startRefreshLocked()
		c.mu.Unlock()
		return
	}
	c.dirty = c.gen != startedGen
	c.refreshing = false
	c.mu.Unlock()
}

func (c *sandboxCache) refreshContext() (context.Context, func()) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	if c.draining == nil {
		return ctx, cancel
	}
	stop := make(chan struct{})
	go func() {
		select {
		case <-c.draining:
			cancel()
		case <-stop:
		}
	}()
	return ctx, func() {
		close(stop)
		cancel()
	}
}
