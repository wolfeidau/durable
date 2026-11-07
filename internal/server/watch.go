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
}

func (f *follower) run(ctx context.Context) error {
	for {
		if f.sess.lost() {
			return errLost()
		}
		if err := f.flushDeltas(ctx); err != nil {
			return err
		}
		n, err := f.sendEvents(ctx)
		if err != nil {
			return err
		}
		if n == watchPage {
			continue
		}
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

// sendEvents pages events after the cursor to the client and reports how many it sent.
func (f *follower) sendEvents(ctx context.Context) (int, error) {
	events, err := f.h.Events(ctx, f.cursor, watchPage)
	if err != nil {
		return 0, fmt.Errorf("failed to read events: %w", err)
	}
	for _, e := range events {
		if err := f.stream.Send(&durablev1.WatchEventsResponse{Item: &durablev1.WatchEventsResponse_Event{Event: wire.Event(e, f.epoch)}}); err != nil {
			return 0, err
		}
		f.cursor = e.Seq
	}
	return len(events), nil
}

func (f *follower) sendHeartbeat() error {
	return f.stream.Send(&durablev1.WatchEventsResponse{Item: &durablev1.WatchEventsResponse_Heartbeat{Heartbeat: &durablev1.Heartbeat{}}})
}

// sendDelta first sends the events the delta follows, so it never arrives ahead of them.
func (f *follower) sendDelta(ctx context.Context, d stampedDelta) error {
	for f.cursor < d.LastSeq {
		n, err := f.sendEvents(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	return f.stream.Send(&durablev1.WatchEventsResponse{Item: &durablev1.WatchEventsResponse_Delta{Delta: wire.Delta(d.Delta, d.LastSeq)}})
}

// flushDeltas sends deltas queued before the next read, so the client sees partial text ahead of the message that replaces it.
func (f *follower) flushDeltas(ctx context.Context) error {
	for range len(f.deltas) {
		if err := f.sendDelta(ctx, <-f.deltas); err != nil {
			return err
		}
	}
	return nil
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
		return f.sendDelta(ctx, d)
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
