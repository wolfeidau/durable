package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	v1 "github.com/wolfeidau/durable/api/durable/v1"
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

func TestOverflowSkipsTextUntilResetButOnlyLosesToolOutputChunks(t *testing.T) {
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
	// Text is skipped even though the buffer now has room, but tool output gets through.
	s.onDelta(textDelta("lost"))
	s.onDelta(durable.Delta{Kind: durable.DeltaToolOutput, Submission: 1, CallID: "c", Text: "out"})
	require.Equal(t, []string{"out"}, drain(slow))
	// A reset is delivered and lets text through again.
	s.onDelta(durable.Delta{Kind: durable.DeltaReset, Submission: 1})
	s.onDelta(textDelta("again"))
	require.Equal(t, []string{"", "again"}, drain(slow))
}

func TestToolOutputOverflowDoesNotSkipLaterOutput(t *testing.T) {
	s := testSession()
	ch, unsub := s.subscribeDeltas()
	defer unsub()
	out := durable.Delta{Kind: durable.DeltaToolOutput, Submission: 1, CallID: "c", Text: "o"}
	for range deltaBuffer + 5 {
		s.onDelta(out)
	}
	require.Len(t, drain(ch), deltaBuffer)
	s.onDelta(out)
	s.onDelta(textDelta("t"))
	require.Equal(t, []string{"o", "t"}, drain(ch))
}

// scriptedFollower feeds a follower fixed events and records what it sends.
func scriptedFollower(events []durable.Event, deltas []stampedDelta) (*follower, *[]string) {
	ch := make(chan stampedDelta, len(deltas))
	for _, d := range deltas {
		ch <- d
	}
	var sent []string
	f := &follower{
		deltas: ch,
		send: func(r *v1.WatchEventsResponse) error {
			if e := r.GetEvent(); e != nil {
				sent = append(sent, fmt.Sprintf("e%d", e.GetSeq()))
			} else {
				sent = append(sent, r.GetDelta().GetText())
			}
			return nil
		},
		events: func(_ context.Context, after int64, limit int) ([]durable.Event, error) {
			var out []durable.Event
			for _, e := range events {
				if e.Seq > after && len(out) < limit {
					out = append(out, e)
				}
			}
			return out, nil
		},
	}
	return f, &sent
}

func TestFollowerMergesDeltasByLastSeq(t *testing.T) {
	events := []durable.Event{{Seq: 1}, {Seq: 2}, {Seq: 3}, {Seq: 4}}
	stamp := func(text string, seq int64) stampedDelta {
		return stampedDelta{Delta: textDelta(text), LastSeq: seq}
	}
	// a and b were produced after event 2 committed and c after event 3, the message they precede.
	f, sent := scriptedFollower(events, []stampedDelta{stamp("a", 2), stamp("b", 2), stamp("c", 3)})
	n, err := f.sendEvents(t.Context())
	require.NoError(t, err)
	require.Equal(t, 4, n)
	require.Equal(t, []string{"e1", "e2", "a", "b", "e3", "c", "e4"}, *sent)
	require.Empty(t, f.pending)
}

func TestFollowerHoldsDeltasAheadOfUnreadEvents(t *testing.T) {
	// The delta follows event 2, which this read hasn't seen yet.
	f, sent := scriptedFollower([]durable.Event{{Seq: 1}}, []stampedDelta{{Delta: textDelta("a"), LastSeq: 2}})
	_, err := f.sendEvents(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"e1"}, *sent)
	require.Len(t, f.pending, 1)
}

func TestFollowerBoundsPendingDuringCatchUp(t *testing.T) {
	f, _ := scriptedFollower(nil, nil)
	ahead := stampedDelta{Delta: textDelta("t"), LastSeq: 100}
	for range deltaBuffer + 10 {
		f.queue(ahead)
	}
	require.Len(t, f.pending, deltaBuffer)
	require.True(t, f.skipText)
	f.queue(stampedDelta{Kind: durable.DeltaToolOutput, Text: "o", LastSeq: 100})
	require.Len(t, f.pending, deltaBuffer, "a full queue drops tool output without skipping later output")
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
