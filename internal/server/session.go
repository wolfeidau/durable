package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/commands"
)

// Session is an open harness with a progress notifier and a run driver.
type Session struct {
	logger *slog.Logger
	h      *durable.Harness
	asm    *commands.Assembled

	mu         sync.Mutex
	newest     int64
	changed    chan struct{} // closed and replaced when newest advances
	kicked     bool
	running    bool
	lastActive time.Time

	kick      chan struct{}
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
}

func newSession(logger *slog.Logger) *Session {
	return &Session{
		logger:     logger,
		changed:    make(chan struct{}),
		kick:       make(chan struct{}, 1),
		done:       make(chan struct{}),
		lastActive: time.Now(),
	}
}

// Harness is the open harness. Use it for reads and for Submit, Decide and Cancel, and call Kick after a change that needs a run.
func (s *Session) Harness() *durable.Harness { return s.h }

// Epoch identifies this copy of the session.
func (s *Session) Epoch() string { return s.h.Epoch() }

// observe is the harness Observer: it only records the seq and wakes waiters, so it never blocks or calls the harness.
func (s *Session) observe(e durable.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Seq > s.newest {
		s.newest = e.Seq
		close(s.changed)
		s.changed = make(chan struct{})
	}
}

// Wait returns once an event newer than afterSeq has committed, or ctx ends.
// Callers re-read events afterwards: it signals progress and carries no data.
func (s *Session) Wait(ctx context.Context, afterSeq int64) error {
	for {
		s.mu.Lock()
		if s.newest > afterSeq {
			s.mu.Unlock()
			return nil
		}
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Kick asks the driver to call Run. It never blocks, and kicks made during a run coalesce into one more run.
func (s *Session) Kick() {
	s.mu.Lock()
	s.kicked = true
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastActive = time.Now()
	s.mu.Unlock()
}

// idleFor is zero while a run is in progress or pending.
func (s *Session) idleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kicked || s.running {
		return 0
	}
	return time.Since(s.lastActive)
}

// start seeds the notifier from committed events, then starts the driver with a first kick to resume unfinished work.
func (s *Session) start(ctx context.Context, h *durable.Harness, asm *commands.Assembled) error {
	s.h, s.asm = h, asm
	var after int64
	for {
		events, err := h.Events(ctx, after, 1000)
		if err != nil {
			_ = h.Close()
			asm.Close()
			return err
		}
		if len(events) == 0 {
			break
		}
		after = events[len(events)-1].Seq
	}
	s.observe(durable.Event{Seq: after})

	ctx, s.cancel = context.WithCancel(ctx)
	go s.drive(ctx)
	s.Kick()
	return nil
}

func (s *Session) drive(ctx context.Context) {
	defer close(s.done)
	for {
		select {
		case <-s.kick:
		case <-ctx.Done():
			return
		}
		s.mu.Lock()
		s.kicked, s.running = false, true
		s.mu.Unlock()

		err := s.h.Run(ctx)
		if err != nil && !errors.Is(err, durable.ErrAwaitingApproval) && ctx.Err() == nil {
			s.logger.Error("run failed", slog.Any("error", err))
		}

		s.mu.Lock()
		s.running = false
		s.lastActive = time.Now()
		s.mu.Unlock()
	}
}

// close stops the driver, then closes the harness and the MCP pool.
func (s *Session) close() {
	s.closeOnce.Do(func() {
		s.cancel()
		<-s.done
		if err := s.h.Close(); err != nil {
			s.logger.Error("failed to close session", slog.Any("error", err))
		}
		s.asm.Close()
	})
}
