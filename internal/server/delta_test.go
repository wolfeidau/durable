package server

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
)

func testSession() *Session {
	return newSession(slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond, time.Millisecond)
}

func textDelta(s string) durable.Delta {
	return durable.Delta{Kind: durable.DeltaText, Submission: 1, Text: s}
}

func drain(ch <-chan stampedDelta) []string {
	var out []string
	for {
		select {
		case d := <-ch:
			out = append(out, d.Text)
		default:
			return out
		}
	}
}

func TestOnDeltaFansOutWithLastSeq(t *testing.T) {
	s := testSession()
	s.observe(durable.Event{Seq: 7})
	a, unsubA := s.subscribeDeltas()
	b, unsubB := s.subscribeDeltas()
	defer unsubA()
	defer unsubB()

	s.onDelta(textDelta("x"))
	for _, ch := range []<-chan stampedDelta{a, b} {
		d := <-ch
		require.Equal(t, "x", d.Text)
		require.Equal(t, int64(7), d.LastSeq)
	}
}

func TestOnDeltaWithNoSubscribersIsNoop(t *testing.T) {
	s := testSession()
	s.onDelta(textDelta("x"))
	require.Empty(t, s.subs)
}

func TestFullBufferMakesSubscriberLossyUntilReset(t *testing.T) {
	s := testSession()
	slow, unsubSlow := s.subscribeDeltas()
	fast, unsubFast := s.subscribeDeltas()
	defer unsubSlow()
	defer unsubFast()

	// The fast watcher keeps up, so it must get everything.
	for range deltaBuffer + 10 {
		s.onDelta(textDelta("t"))
		require.Equal(t, "t", (<-fast).Text)
	}

	require.Len(t, drain(slow), deltaBuffer)
	// Lossy: text is skipped even though the buffer now has room.
	s.onDelta(textDelta("lost"))
	require.Empty(t, drain(slow))
	// A reset is delivered and clears the flag.
	s.onDelta(durable.Delta{Kind: durable.DeltaReset, Submission: 1})
	s.onDelta(textDelta("again"))
	require.Equal(t, []string{"", "again"}, drain(slow))
}

func TestOnDeltaDoesNotBlockWithStalledSubscriber(t *testing.T) {
	s := testSession()
	_, unsub := s.subscribeDeltas()
	defer unsub()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range deltaBuffer * 3 {
			s.onDelta(textDelta("t"))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("onDelta blocked")
	}
}

func TestUnsubscribeIsSafeTwice(t *testing.T) {
	s := testSession()
	_, unsub := s.subscribeDeltas()
	unsub()
	unsub()
	s.onDelta(textDelta("x"))
	require.Empty(t, s.subs)
}
