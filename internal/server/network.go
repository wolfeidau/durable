package server

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/internal/audit"
	"github.com/wolfeidau/durable/internal/sandbox"
)

const (
	defaultNetworkSince = 24 * time.Hour
	// maxNetworkLimit is also the default, and keeps one response bounded.
	maxNetworkLimit = 1000
)

// ListNetworkEvents only reads the pin and the gateway log, so it never opens the sandbox or the session.
func (s sessionService) ListNetworkEvents(ctx context.Context, req *connect.Request[v1.ListNetworkEventsRequest]) (*connect.Response[v1.ListNetworkEventsResponse], error) {
	msg := req.Msg
	dir, err := SessionDir(s.home, msg.GetSessionId())
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("unknown session"))
	} else if err != nil {
		return nil, s.internal(ctx, "stat session", err)
	}
	pin, ok, err := sandbox.ReadPin(dir)
	if err != nil {
		return nil, s.internal(ctx, "read sandbox pin", err)
	}
	if !ok {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("session has no sandbox, so it has no network events"))
	}
	now := time.Now()
	since, err := networkSince(msg, now)
	if err != nil {
		return nil, err
	}
	limit := int(msg.GetLimit())
	switch {
	case limit < 0 || limit > maxNetworkLimit:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("limit must be between 0 and 1000"))
	case limit == 0:
		limit = maxNetworkLimit
	}
	res, err := audit.Read(ctx, audit.Query{
		Gateway: s.gateway, Sandbox: pin.Name, Since: since, After: msg.GetAfter(), Limit: limit,
	})
	if errors.Is(err, audit.ErrBadCursor) {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err != nil {
		return nil, s.internal(ctx, "read network events", err)
	}
	return connect.NewResponse(&v1.ListNetworkEventsResponse{
		Events:    networkEvents(res.Events),
		Coverage:  s.networkCoverage(ctx, res.Coverage, since),
		Truncated: res.Truncated,
	}), nil
}

func networkSince(msg *v1.ListNetworkEventsRequest, now time.Time) (time.Time, error) {
	switch w := msg.GetSince().(type) {
	case *v1.ListNetworkEventsRequest_SinceAgo:
		if err := w.SinceAgo.CheckValid(); err != nil || w.SinceAgo.AsDuration() <= 0 {
			return time.Time{}, connect.NewError(connect.CodeInvalidArgument, errors.New("since_ago must be a positive duration"))
		}
		return now.Add(-w.SinceAgo.AsDuration()), nil
	case *v1.ListNetworkEventsRequest_SinceTime:
		if err := w.SinceTime.CheckValid(); err != nil {
			return time.Time{}, connect.NewError(connect.CodeInvalidArgument, errors.New("since_time is not a valid timestamp"))
		}
		return w.SinceTime.AsTime(), nil
	}
	return now.Add(-defaultNetworkSince), nil
}

func networkEvents(events []audit.Event) []*v1.NetworkEvent {
	out := make([]*v1.NetworkEvent, len(events))
	for i, e := range events {
		out[i] = &v1.NetworkEvent{
			Time: timestamppb.New(e.Time), Id: e.ID, Allowed: e.Allowed, Kind: e.Kind, Activity: e.Activity, Binary: e.Binary,
			Method: e.Method, Host: e.Host, Path: e.Path, Pattern: e.Pattern(), Policy: e.Policy, Reason: e.Reason,
		}
	}
	return out
}

func (s sessionService) networkCoverage(ctx context.Context, c audit.Coverage, since time.Time) *v1.NetworkCoverage {
	out := &v1.NetworkCoverage{
		Source:          v1.NetworkSource_NETWORK_SOURCE_GATEWAY_LOG,
		SourceBounded:   true,
		GatewayLogStart: optTime(c.GatewayStart),
		Since:           timestamppb.New(since),
	}
	if c.GatewayErr != nil {
		// Gateway errors can name providers, so only the log gets the cause.
		s.logger.WarnContext(ctx, "network events: gateway log unreadable", slog.Any("error", c.GatewayErr))
		out.GatewayError = "the gateway log could not be read: see the server log"
	}
	return out
}

func optTime(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
