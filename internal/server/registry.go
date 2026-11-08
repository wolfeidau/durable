package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/commands"
)

// DefaultIdleTimeout is how long an unused session stays open.
const DefaultIdleTimeout = 5 * time.Minute

var errRegistryClosed = errors.New("session registry is closed")

// errCreate marks a failure to assemble a new session, such as a misconfigured provider.
var errCreate = errors.New("failed to assemble new session")

// RegistryConfig configures a Registry.
type RegistryConfig struct {
	Home string
	// IdleTimeout closes a session with no holders, run or pending kick for this long. Zero means DefaultIdleTimeout.
	IdleTimeout time.Duration
	// OpenTimeout bounds opening a session, which waits while another process holds its lease. Zero means 10s.
	OpenTimeout time.Duration
	// APIToken is the resolved provider key; empty lets Assemble use the provider's environment variable.
	APIToken string
	// Provider, Protocol and Model apply to sessions created through Create; reopening uses the session's pin.
	Provider string
	Protocol string
	Model    string
	// RetryBase and RetryCap bound the delay before a failed run is retried; the delay doubles from base. Zero means 5s and 5m.
	RetryBase time.Duration
	RetryCap  time.Duration
	Logger    *slog.Logger

	// assemble is a test seam.
	assemble func(context.Context, commands.AssembleConfig) (*commands.Assembled, error)
}

// Registry opens sessions on demand, drives their runs and closes idle ones.
type Registry struct {
	cfg    RegistryConfig
	logger *slog.Logger

	runCtx    context.Context //nolint:containedctx // the run context outlives any request and is owned by the registry
	cancelRun context.CancelFunc
	janitor   chan struct{}

	mu      sync.Mutex
	closed  bool
	entries map[string]*entry
}

type entry struct {
	ready   chan struct{} // closed once s or err is set
	s       *Session
	err     error
	holders int  // guarded by Registry.mu
	evicted bool // guarded by Registry.mu; the last release closes the session
}

// NewRegistry returns a Registry whose runs use a context derived from ctx.
// ctx should outlive request contexts: cancel it only after the HTTP server has shut down.
func NewRegistry(ctx context.Context, cfg RegistryConfig) *Registry {
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = 5 * time.Second
	}
	if cfg.RetryCap <= 0 {
		cfg.RetryCap = 5 * time.Minute
	}
	if cfg.OpenTimeout <= 0 {
		cfg.OpenTimeout = 10 * time.Second
	}
	if cfg.assemble == nil {
		cfg.assemble = commands.Assemble
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	runCtx, cancel := context.WithCancel(ctx)
	r := &Registry{cfg: cfg, logger: logger, runCtx: runCtx, cancelRun: cancel, janitor: make(chan struct{}), entries: map[string]*entry{}}
	go r.sweepLoop()
	return r
}

// Acquire returns the open session for id, opening it if needed, and a release func that must be called once.
// Concurrent calls for one id share one Session.
func (r *Registry) Acquire(ctx context.Context, id string) (*Session, func(), error) {
	dir, err := SessionDir(r.cfg.Home, id)
	if err != nil {
		return nil, nil, err
	}
	return r.acquire(ctx, id, dir, nil)
}

// createSpec makes open create the session instead of requiring it to exist.
type createSpec struct{ cwd, name string }

// Create returns the session for cwd and name, creating and opening it if it doesn't exist.
// created is false for an existing session, which is returned as it is.
func (r *Registry) Create(ctx context.Context, cwd, name string) (meta commands.SessionMeta, created bool, err error) {
	path, err := commands.SessionPath(cwd, name)
	if err != nil {
		return commands.SessionMeta{}, false, err
	}
	dir := filepath.Dir(path)
	id := filepath.Base(dir)
	meta, err = commands.ReadSessionMeta(dir)
	if err == nil {
		meta.ID = id
		return meta, false, nil
	}
	if !errors.Is(err, commands.ErrNoSession) {
		return commands.SessionMeta{}, false, err
	}
	_, release, err := r.acquire(ctx, id, dir, &createSpec{cwd: cwd, name: name})
	if err != nil {
		return commands.SessionMeta{}, false, err
	}
	release()
	meta, err = commands.ReadSessionMeta(dir)
	if err != nil {
		return commands.SessionMeta{}, false, err
	}
	meta.ID = id
	return meta, true, nil
}

// acquire opens the session with spec only if no caller has it open yet.
func (r *Registry) acquire(ctx context.Context, id, dir string, spec *createSpec) (*Session, func(), error) {
	for {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return nil, nil, errRegistryClosed
		}
		e, ok := r.entries[id]
		if !ok {
			e = &entry{ready: make(chan struct{})}
			r.entries[id] = e
		}
		e.holders++
		r.mu.Unlock()

		if !ok {
			r.finishOpen(ctx, id, dir, e, spec)
		}

		select {
		case <-e.ready:
		case <-ctx.Done():
			r.release(e)
			return nil, nil, ctx.Err()
		}
		if e.err != nil {
			r.release(e)
			// The leader's own cancellation says nothing about this caller's context.
			// A creator that joined a lookup of a missing session leads the next attempt.
			if ctx.Err() == nil && (errors.Is(e.err, context.Canceled) || errors.Is(e.err, context.DeadlineExceeded) || (spec != nil && errors.Is(e.err, commands.ErrNoSession))) {
				continue
			}
			return nil, nil, e.err
		}
		if e.s.lost() {
			r.mu.Lock()
			r.evictLocked(id, e)
			r.mu.Unlock()
			r.release(e)
			continue
		}
		var once sync.Once
		return e.s, func() { once.Do(func() { r.release(e) }) }, nil
	}
}

// finishOpen opens the session for the entry's first caller and wakes the others.
func (r *Registry) finishOpen(ctx context.Context, id, dir string, e *entry, spec *createSpec) {
	s, err := r.open(ctx, dir, spec)
	r.mu.Lock()
	// Close can't see an entry with no session, so the opener closes it.
	lateClose := err == nil && r.closed
	if lateClose {
		err = errRegistryClosed
	}
	e.err = err
	if err == nil {
		e.s = s
	} else {
		delete(r.entries, id)
	}
	r.mu.Unlock()
	if lateClose {
		s.close()
	}
	close(e.ready)
}

// evictLocked removes e from the map if it is still the current entry for id.
func (r *Registry) evictLocked(id string, e *entry) {
	if r.entries[id] == e {
		delete(r.entries, id)
	}
	e.evicted = true
}

// release closes an evicted session once its last holder leaves.
func (r *Registry) release(e *entry) {
	r.mu.Lock()
	e.holders--
	var closing *Session
	if e.s != nil {
		e.s.touch()
		if e.evicted && e.holders == 0 {
			closing = e.s
		}
	}
	r.mu.Unlock()
	if closing != nil {
		closing.close()
	}
}

func (r *Registry) open(ctx context.Context, dir string, spec *createSpec) (*Session, error) {
	s := newSession(r.logger.With(slog.String("session", filepath.Base(dir))), r.cfg.RetryBase, r.cfg.RetryCap)
	ac := commands.AssembleConfig{
		Home:        r.cfg.Home,
		APIToken:    r.cfg.APIToken,
		Interactive: false,
		Observer:    s.observe,
	}
	if spec != nil {
		ac.CWD, ac.Session = spec.cwd, spec.name
		ac.Provider, ac.Protocol, ac.Model = r.cfg.Provider, r.cfg.Protocol, r.cfg.Model
	} else {
		meta, err := commands.ReadSessionMeta(dir)
		if err != nil {
			return nil, notFound(err)
		}
		// No provider fields: route.matches rejects a flag that differs from the session's pin.
		ac.CWD, ac.Session, ac.ExistingOnly = meta.CWD, meta.Name, true
	}
	asm, err := r.cfg.assemble(ctx, ac)
	if err != nil {
		if spec != nil && ctx.Err() == nil {
			return nil, fmt.Errorf("%w: %w", errCreate, err)
		}
		return nil, notFound(err)
	}
	asm.Options.OnDelta = s.onDelta
	// The cwd and name hash to the directory, so a forged session.json can't point at another session.
	if filepath.Dir(asm.Path) != dir {
		asm.Close()
		return nil, fmt.Errorf("session.json does not match directory %s", filepath.Base(dir))
	}
	// Only the lease wait is bounded: a slow Assemble step must not read as a held session.
	octx, cancel := context.WithTimeout(ctx, r.cfg.OpenTimeout)
	defer cancel()
	h, err := durable.Open(octx, asm.Path, asm.Options)
	if err != nil {
		asm.Close()
		if ctx.Err() == nil && octx.Err() != nil {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("session in use"))
		}
		return nil, fmt.Errorf("failed to open session: %w", err)
	}
	if err := s.start(r.runCtx, h, asm); err != nil {
		return nil, err
	}
	return s, nil
}

func notFound(err error) error {
	if errors.Is(err, commands.ErrNoSession) {
		return connect.NewError(connect.CodeNotFound, err)
	}
	return err
}

// sweepLoop closes sessions that have been idle for IdleTimeout, and lost ones at once.
func (r *Registry) sweepLoop() {
	interval := max(r.cfg.IdleTimeout/4, 5*time.Millisecond)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.sweep()
		case <-r.janitor:
			return
		}
	}
}

func (r *Registry) sweep() {
	var idle []*Session
	r.mu.Lock()
	for id, e := range r.entries {
		if e.s == nil {
			continue
		}
		switch {
		case e.s.lost():
			// With holders, the last release closes it.
			r.evictLocked(id, e)
			if e.holders == 0 {
				idle = append(idle, e.s)
			}
		case e.holders == 0 && e.s.idleFor() >= r.cfg.IdleTimeout:
			r.evictLocked(id, e)
			idle = append(idle, e.s)
		}
	}
	r.mu.Unlock()
	// Closing waits for the driver, so it runs outside the lock.
	for _, s := range idle {
		s.close()
	}
}

// Close cancels runs, waits for the drivers to return, then closes every session.
// Call it after the HTTP server has shut down.
func (r *Registry) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	var all []*Session
	for id, e := range r.entries {
		// An entry still opening is closed by its opener.
		if e.s != nil {
			all = append(all, e.s)
			delete(r.entries, id)
		}
	}
	r.mu.Unlock()

	close(r.janitor)
	r.cancelRun()
	for _, s := range all {
		s.close()
	}
}
