package server

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/wolfeidau/durable"
	v1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/internal/commands"
	"github.com/wolfeidau/durable/internal/wire"
)

func (s sessionService) Submit(ctx context.Context, req *connect.Request[v1.SubmitRequest]) (*connect.Response[v1.SubmitResponse], error) {
	if req.Msg.GetRequestId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("request_id is required"))
	}
	sess, release, err := s.reg.Acquire(ctx, req.Msg.GetSessionId())
	if err != nil {
		return nil, err
	}
	defer release()
	sub, err := sess.Harness().Submit(ctx, req.Msg.GetRequestId(), req.Msg.GetText())
	if err != nil {
		return nil, s.internal(ctx, "submit", err)
	}
	sess.Kick()
	return connect.NewResponse(&v1.SubmitResponse{Submission: wire.Submission(sub)}), nil
}

func (s sessionService) Cancel(ctx context.Context, req *connect.Request[v1.CancelRequest]) (*connect.Response[v1.CancelResponse], error) {
	sess, release, err := s.reg.Acquire(ctx, req.Msg.GetSessionId())
	if err != nil {
		return nil, err
	}
	defer release()
	sub, err := sess.Harness().Cancel(ctx, req.Msg.GetRequestId())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("unknown request"))
		}
		return nil, s.internal(ctx, "cancel", err)
	}
	return connect.NewResponse(&v1.CancelResponse{Submission: wire.Submission(sub)}), nil
}

func (s sessionService) DecideApproval(ctx context.Context, req *connect.Request[v1.DecideApprovalRequest]) (*connect.Response[v1.DecideApprovalResponse], error) {
	sess, release, err := s.reg.Acquire(ctx, req.Msg.GetSessionId())
	if err != nil {
		return nil, err
	}
	defer release()
	a, err := sess.Harness().Decide(ctx, req.Msg.GetApprovalSeq(), req.Msg.GetAllow(), req.Msg.GetBy())
	switch {
	case errors.Is(err, durable.ErrApprovalDecided):
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("approval already decided differently"))
	case errors.Is(err, sql.ErrNoRows):
		return nil, connect.NewError(connect.CodeNotFound, errors.New("unknown approval"))
	case err != nil:
		return nil, s.internal(ctx, "decide approval", err)
	}
	// A repeated decision is a no-op in Decide, and Run finds nothing to do after it.
	sess.Kick()
	return connect.NewResponse(&v1.DecideApprovalResponse{Approval: wire.Approval(a)}), nil
}

func (s sessionService) ListSessions(ctx context.Context, _ *connect.Request[v1.ListSessionsRequest]) (*connect.Response[v1.ListSessionsResponse], error) {
	metas, warnings, err := commands.ListSessions(s.home)
	if err != nil {
		return nil, s.internal(ctx, "list sessions", err)
	}
	out := &v1.ListSessionsResponse{Warnings: warnings}
	for _, m := range metas {
		out.Sessions = append(out.Sessions, &v1.SessionInfo{Id: m.ID, Cwd: m.CWD, Name: m.Name, CreatedAt: timestamppb.New(m.CreatedAt)})
	}
	return connect.NewResponse(out), nil
}

// internal logs err and returns a code-only error, so internals don't reach the client.
func (s sessionService) internal(ctx context.Context, op string, err error) error {
	s.logger.ErrorContext(ctx, "request failed", slog.String("op", op), slog.Any("error", err))
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}
