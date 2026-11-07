package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	"github.com/wolfeidau/durable"
	durablev1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/internal/wire"
)

// DefaultHeartbeat is how often an idle watch stream sends a heartbeat.
const DefaultHeartbeat = 20 * time.Second

// watchPage is a var so a test can exercise paging without hundreds of events.
var watchPage = 256

var errDraining = errors.New("server shutting down")

// WatchEvents replays committed events after the cursor, then follows new ones on the same read path.
func (s sessionService) WatchEvents(ctx context.Context, req *connect.Request[durablev1.WatchEventsRequest], stream *connect.ServerStream[durablev1.WatchEventsResponse]) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		select {
		case <-s.draining:
			cancel(errDraining)
		case <-ctx.Done():
		}
	}()
	return s.watchErr(ctx, s.watch(ctx, req.Msg, stream))
}

func (s sessionService) watch(ctx context.Context, req *durablev1.WatchEventsRequest, stream *connect.ServerStream[durablev1.WatchEventsResponse]) error {
	sess, release, err := s.reg.Acquire(ctx, req.GetSessionId())
	if err != nil {
		return err
	}
	// Holding the session keeps it from closing as idle while the stream is open.
	defer release()

	epoch := sess.Epoch()
	cursor := req.GetAfterSeq()
	if cursor < 0 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("after_seq must not be negative"))
	}
	// A cursor with no epoch can't be trusted across a restore, so it replays from 0.
	staleEpoch := req.GetEpoch() != epoch && (req.GetEpoch() != "" || cursor > 0)
	if newest := sess.newestSeq(); staleEpoch || cursor > newest {
		return epochMismatch(epoch, newest)
	}

	// Headers go out now, so a client's call returns without waiting for the first event or heartbeat.
	if err := stream.Send(nil); err != nil {
		return err
	}
	f := &follower{
		sess: sess, h: sess.Harness(), stream: stream, epoch: epoch, cursor: cursor, heartbeat: s.heartbeat,
	}
	if req.GetIncludeDeltas() {
		// Subscribing before the first read means no delta committed after the catch-up is missed.
		var unsub func()
		f.deltas, unsub = sess.subscribeDeltas()
		defer unsub()
	}
	return f.run(ctx)
}

// follower streams one session's events, and its deltas when subscribed, to one client.
type follower struct {
	sess      *Session
	h         *durable.Harness
	stream    *connect.ServerStream[durablev1.WatchEventsResponse]
	epoch     string
	cursor    int64
	heartbeat time.Duration
	deltas    <-chan stampedDelta // nil without include_deltas
	pending   []stampedDelta      // drained deltas not yet sent, in LastSeq order
}

func (f *follower) run(ctx context.Context) error {
	for {
		if f.sess.lost() {
			return errLost()
		}
		if f.deltaOnly() {
			// Nothing new committed, so skip the read and keep a token stream off the single connection.
			f.drainDeltas()
			if err := f.sendPending(f.cursor); err != nil {
				return err
			}
		} else {
			n, err := f.sendEvents(ctx)
			if err != nil {
				return err
			}
			if n == watchPage {
				continue
			}
		}
		var err error
		if f.deltas == nil {
			err = f.wait(ctx)
		} else {
			err = f.waitWithDeltas(ctx)
		}
		if err != nil {
			return err
		}
	}
}

// sendEvents pages events after the cursor to the client, merging in deltas by LastSeq, and reports how many events it sent.
func (f *follower) sendEvents(ctx context.Context) (int, error) {
	events, err := f.h.Events(ctx, f.cursor, watchPage)
	if err != nil {
		return 0, fmt.Errorf("failed to read events: %w", err)
	}
	// Drained after the read, so every delta produced before these events committed is pending.
	f.drainDeltas()
	for _, e := range events {
		if err := f.sendPending(e.Seq - 1); err != nil {
			return 0, err
		}
		if err := f.stream.Send(&durablev1.WatchEventsResponse{Item: &durablev1.WatchEventsResponse_Event{Event: wire.Event(e, f.epoch)}}); err != nil {
			return 0, err
		}
		f.cursor = e.Seq
	}
	return len(events), f.sendPending(f.cursor)
}

// deltaOnly reports a wake-up for deltas alone, with no event committed after the cursor.
func (f *follower) deltaOnly() bool {
	if len(f.pending) == 0 {
		return false
	}
	_, ready := f.sess.progress(f.cursor)
	return !ready
}

func (f *follower) drainDeltas() {
	for range len(f.deltas) {
		f.pending = append(f.pending, <-f.deltas)
	}
}

// sendPending sends pending deltas that follow no event after upTo, so a delta never arrives ahead of the events it follows or behind one it precedes.
func (f *follower) sendPending(upTo int64) error {
	i := 0
	for ; i < len(f.pending) && f.pending[i].LastSeq <= upTo; i++ {
		d := f.pending[i]
		if err := f.stream.Send(&durablev1.WatchEventsResponse{Item: &durablev1.WatchEventsResponse_Delta{Delta: wire.Delta(d.Delta, d.LastSeq)}}); err != nil {
			return err
		}
	}
	f.pending = f.pending[i:]
	return nil
}

func (f *follower) sendHeartbeat() error {
	return f.stream.Send(&durablev1.WatchEventsResponse{Item: &durablev1.WatchEventsResponse_Heartbeat{Heartbeat: &durablev1.Heartbeat{}}})
}

// wait blocks until progress, sending a heartbeat on timeout.
func (f *follower) wait(ctx context.Context) error {
	wctx, cancel := context.WithTimeout(ctx, f.heartbeat)
	err := f.sess.Wait(wctx, f.cursor)
	cancel()
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	default:
		return f.sendHeartbeat()
	}
}

// waitWithDeltas is wait that also wakes for a delta.
func (f *follower) waitWithDeltas(ctx context.Context) error {
	changed, ready := f.sess.progress(f.cursor)
	if ready {
		return nil
	}
	timer := time.NewTimer(f.heartbeat)
	defer timer.Stop()
	select {
	case <-changed:
		return nil
	case d := <-f.deltas:
		f.pending = append(f.pending, d)
		return nil
	case <-timer.C:
		return f.sendHeartbeat()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// watchErr reports a shutdown as Unavailable so clients reconnect.
func (s sessionService) watchErr(ctx context.Context, err error) error {
	if err != nil && errors.Is(context.Cause(ctx), errDraining) {
		return connect.NewError(connect.CodeUnavailable, errDraining)
	}
	if err == nil {
		return nil
	}
	return s.publicErr(ctx, "watch", err)
}

func epochMismatch(epoch string, lastSeq int64) error {
	err := connect.NewError(connect.CodeFailedPrecondition, errors.New("epoch mismatch: replay from 0"))
	detail, derr := connect.NewErrorDetail(&durablev1.EpochMismatch{Epoch: epoch, LastSeq: lastSeq})
	if derr != nil {
		return fmt.Errorf("failed to encode epoch mismatch: %w", derr)
	}
	err.AddDetail(detail)
	return err
}
