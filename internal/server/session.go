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
	retrying   bool // a re-kick is scheduled after a failed run
	isLost     bool // the lease was taken by another process
	lastActive time.Time

	retryBase, retryCap time.Duration
	retryDelay          time.Duration // owned by the driver
	retryTimer          *time.Timer   // guarded by mu

	runCancel context.CancelFunc // guarded by mu; the in-progress run's cancel
	pauses    int                // guarded by mu; CancelSubmissions in progress
	pause     chan struct{}      // guarded by mu; closed when pauses returns to zero
	kick      chan struct{}
	cancel    context.CancelFunc
	done      chan struct{}
	watchDone chan struct{} // closed when the lease watcher exits
	closeOnce sync.Once
}

func newSession(logger *slog.Logger, retryBase, retryCap time.Duration) *Session {
	return &Session{
		retryBase:  retryBase,
		retryCap:   retryCap,
		logger:     logger,
		changed:    make(chan struct{}),
		kick:       make(chan struct{}, 1),
		done:       make(chan struct{}),
		watchDone:  make(chan struct{}),
		lastActive: time.Now(),
	}
}

// Harness is the open harness. Use it for reads and for Submit, Decide and Cancel, and call Kick after a change that needs a run.
func (s *Session) Harness() *durable.Harness { return s.h }

// Epoch identifies this copy of the session.
func (s *Session) Epoch() string { return s.h.Epoch() }

// newestSeq is the newest committed seq this session has seen.
func (s *Session) newestSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newest
}

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
// It also returns once the session is lost, so callers check lost() after a wake-up.
// Callers re-read events afterwards: it signals progress and carries no data.
func (s *Session) Wait(ctx context.Context, afterSeq int64) error {
	for {
		s.mu.Lock()
		if s.newest > afterSeq || s.isLost {
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

// markLost records that another process took the lease and wakes Wait callers.
func (s *Session) markLost() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isLost {
		return
	}
	s.isLost = true
	s.logger.Warn("session taken over by another process")
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Session) lost() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isLost
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastActive = time.Now()
	s.mu.Unlock()
}

// idleFor is zero while a run is in progress or pending; a scheduled retry doesn't count, so an outage can't hold the lease forever.
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
	go s.watchLease(ctx)
	s.Kick()
	return nil
}

// watchLease marks the session lost when background renewal fails, so an idle session doesn't wait for a write to notice.
func (s *Session) watchLease(ctx context.Context) {
	defer close(s.watchDone)
	select {
	case <-s.h.LeaseLost():
		s.markLost()
	case <-ctx.Done():
	}
}

func (s *Session) drive(ctx context.Context) {
	defer close(s.done)
	for {
		select {
		case <-s.kick:
		case <-ctx.Done():
			return
		}
		runCtx, cancel, ok := s.beginRun(ctx)
		if !ok {
			return
		}
		err := s.h.Run(runCtx)
		// An interrupted run is a normal outcome, so only a run nobody cancelled can have failed.
		failed := err != nil && !errors.Is(err, durable.ErrAwaitingApproval) && runCtx.Err() == nil
		cancel()
		if errors.Is(err, durable.ErrLeaseLost) {
			s.mu.Lock()
			s.running = false
			s.runCancel = nil
			s.mu.Unlock()
			s.markLost()
			return
		}
		if failed {
			s.logger.Error("run failed", slog.Any("error", err))
		}

		s.mu.Lock()
		s.running = false
		s.runCancel = nil
		if failed {
			// Only the first failure of a streak counts as activity, so an outage ends in an idle close.
			if s.retryDelay == 0 {
				s.lastActive = time.Now()
			}
			s.scheduleRetryLocked()
		} else {
			s.lastActive = time.Now()
			s.retryDelay = 0
		}
		s.mu.Unlock()
	}
}

// beginRun waits out any pause, then registers the run so Interrupt can cancel it.
func (s *Session) beginRun(ctx context.Context) (context.Context, context.CancelFunc, bool) {
	for {
		s.mu.Lock()
		if p := s.pause; p != nil {
			s.mu.Unlock()
			select {
			case <-p:
				continue
			case <-ctx.Done():
				return nil, nil, false
			}
		}
		runCtx, cancel := context.WithCancel(ctx)
		s.kicked, s.running = false, true
		s.runCancel = cancel
		s.mu.Unlock()
		return runCtx, cancel, true
	}
}

// CancelSubmission cancels a submission, interrupting the run first when it is the one running.
// Harness.Cancel waits for the run slot, so without the interrupt it would wait for the whole run.
func (s *Session) CancelSubmission(ctx context.Context, requestID string) (durable.Submission, error) {
	cur, err := s.h.Get(ctx, requestID)
	if err != nil {
		return durable.Submission{}, err
	}
	if cur.Status == durable.StatusRunning {
		// Pausing keeps the driver from starting a run that would take the slot again.
		s.mu.Lock()
		if s.pauses == 0 {
			s.pause = make(chan struct{})
		}
		s.pauses++
		if s.runCancel != nil {
			s.runCancel()
		}
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			if s.pauses--; s.pauses == 0 {
				close(s.pause)
				s.pause = nil
			}
			s.mu.Unlock()
		}()
	}
	sub, err := s.h.Cancel(ctx, requestID)
	// Cancelling a submission that already finished returns it unchanged, and the queue continues either way.
	s.Kick()
	return sub, err
}

// scheduleRetryLocked re-kicks after a growing delay, so a failed run isn't stranded until a client acts.
func (s *Session) scheduleRetryLocked() {
	if s.retryDelay == 0 {
		s.retryDelay = s.retryBase
	} else {
		s.retryDelay = min(s.retryDelay*2, s.retryCap)
	}
	s.retrying = true
	s.retryTimer = time.AfterFunc(s.retryDelay, func() {
		// Kick first so the session is never briefly idle between the two; after close the kick is never read.
		s.Kick()
		s.mu.Lock()
		s.retrying = false
		s.mu.Unlock()
	})
}

// close stops the driver, then closes the harness and the MCP pool.
func (s *Session) close() {
	s.closeOnce.Do(func() {
		s.cancel()
		<-s.done
		<-s.watchDone
		s.mu.Lock()
		if s.retryTimer != nil {
			s.retryTimer.Stop()
		}
		s.mu.Unlock()
		if err := s.h.Close(); err != nil {
			s.logger.Error("failed to close session", slog.Any("error", err))
		}
		s.asm.Close()
	})
}
