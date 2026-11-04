package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	durablev1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/internal/wire"
)

// DefaultHeartbeat is how often an idle watch stream sends a heartbeat.
const DefaultHeartbeat = 20 * time.Second

const watchPage = 256

// WatchEvents replays committed events after the cursor, then follows new ones on the same read path.
func (s sessionService) WatchEvents(ctx context.Context, req *connect.Request[durablev1.WatchEventsRequest], stream *connect.ServerStream[durablev1.WatchEventsResponse]) error {
	sess, release, err := s.reg.Acquire(ctx, req.Msg.GetSessionId())
	if err != nil {
		return err
	}
	// Holding the session keeps it from closing as idle while the stream is open.
	defer release()

	epoch := sess.Epoch()
	cursor := req.Msg.GetAfterSeq()
	if cursor < 0 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("after_seq must not be negative"))
	}
	if newest := sess.newestSeq(); (req.Msg.GetEpoch() != "" && req.Msg.GetEpoch() != epoch) || cursor > newest {
		return epochMismatch(epoch, newest)
	}

	h := sess.Harness()
	for {
		events, err := h.Events(ctx, cursor, watchPage)
		if err != nil {
			return fmt.Errorf("failed to read events: %w", err)
		}
		for _, e := range events {
			if err := stream.Send(&durablev1.WatchEventsResponse{Item: &durablev1.WatchEventsResponse_Event{Event: wire.Event(e, epoch)}}); err != nil {
				return err
			}
			cursor = e.Seq
		}
		if len(events) == watchPage {
			continue
		}

		wctx, cancel := context.WithTimeout(ctx, s.heartbeat)
		err = sess.Wait(wctx, cursor)
		cancel()
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			if err := stream.Send(&durablev1.WatchEventsResponse{Item: &durablev1.WatchEventsResponse_Heartbeat{Heartbeat: &durablev1.Heartbeat{}}}); err != nil {
				return err
			}
		}
	}
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
