package server

import "github.com/wolfeidau/durable"

// deltaBuffer bounds how far a watcher may lag before it starts losing deltas.
const deltaBuffer = 256

// stampedDelta is a delta with the newest committed seq when it was produced.
type stampedDelta struct {
	durable.Delta
	LastSeq int64
}

type deltaSub struct {
	ch    chan stampedDelta
	lossy bool // guarded by Session.mu; skip deltas until the next reset so a client never shows garbled text
}

// onDelta is the harness OnDelta: it never blocks or calls the harness, and does nothing without subscribers.
func (s *Session) onDelta(d durable.Delta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.subs) == 0 {
		return
	}
	sd := stampedDelta{Delta: d, LastSeq: s.newest}
	for sub := range s.subs {
		if sub.lossy && d.Kind != durable.DeltaReset {
			continue
		}
		select {
		case sub.ch <- sd:
			sub.lossy = false
		default:
			sub.lossy = true
		}
	}
}

// subscribeDeltas returns a channel of deltas and an idempotent unsubscribe.
func (s *Session) subscribeDeltas() (<-chan stampedDelta, func()) {
	sub := &deltaSub{ch: make(chan stampedDelta, deltaBuffer)}
	s.mu.Lock()
	if s.subs == nil {
		s.subs = map[*deltaSub]struct{}{}
	}
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	return sub.ch, func() {
		s.mu.Lock()
		delete(s.subs, sub)
		s.mu.Unlock()
	}
}

// progress returns the channel closed on the next change, and whether afterSeq already has newer events or the session is lost.
func (s *Session) progress(afterSeq int64) (<-chan struct{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed, s.newest > afterSeq || s.isLost
}
