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
	ch       chan stampedDelta
	skipText bool // guarded by Session.mu
}

// admit applies the overflow rule and reports whether to keep d. Missing text garbles a reply, so after an
// overflow text is skipped until the next reset; a gap in tool output is harmless, so only that chunk is lost.
func admit(skipText *bool, kind durable.DeltaKind, full bool) bool {
	if kind == durable.DeltaText && *skipText {
		return false
	}
	if full {
		if kind != durable.DeltaToolOutput {
			*skipText = true
		}
		return false
	}
	if kind == durable.DeltaReset {
		*skipText = false
	}
	return true
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
		// onDelta is the only sender and runs under mu, so a send with room never blocks.
		if admit(&sub.skipText, d.Kind, len(sub.ch) == cap(sub.ch)) {
			sub.ch <- sd
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
