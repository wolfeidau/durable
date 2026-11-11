package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/wolfeidau/durable"
	v1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/internal/commands"
	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/wire"
)

func (s sessionService) Submit(ctx context.Context, req *connect.Request[v1.SubmitRequest]) (*connect.Response[v1.SubmitResponse], error) {
	if req.Msg.GetRequestId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("request_id is required"))
	}
	sess, release, err := s.reg.Acquire(ctx, req.Msg.GetSessionId())
	if err != nil {
		return nil, s.publicErr(ctx, "open session", err)
	}
	defer release()
	sub, err := sess.Harness().Submit(ctx, req.Msg.GetRequestId(), req.Msg.GetText())
	if err != nil {
		if lerr := leaseLost(sess, err); lerr != nil {
			return nil, lerr
		}
		return nil, s.internal(ctx, "submit", err)
	}
	sess.Kick()
	return connect.NewResponse(&v1.SubmitResponse{Submission: wire.Submission(sub)}), nil
}

func (s sessionService) Cancel(ctx context.Context, req *connect.Request[v1.CancelRequest]) (*connect.Response[v1.CancelResponse], error) {
	sess, release, err := s.reg.Acquire(ctx, req.Msg.GetSessionId())
	if err != nil {
		return nil, s.publicErr(ctx, "open session", err)
	}
	defer release()
	sub, err := sess.CancelSubmission(ctx, req.Msg.GetRequestId())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("unknown request"))
		}
		if lerr := leaseLost(sess, err); lerr != nil {
			return nil, lerr
		}
		return nil, s.internal(ctx, "cancel", err)
	}
	return connect.NewResponse(&v1.CancelResponse{Submission: wire.Submission(sub)}), nil
}

func (s sessionService) DecideApproval(ctx context.Context, req *connect.Request[v1.DecideApprovalRequest]) (*connect.Response[v1.DecideApprovalResponse], error) {
	sess, release, err := s.reg.Acquire(ctx, req.Msg.GetSessionId())
	if err != nil {
		return nil, s.publicErr(ctx, "open session", err)
	}
	defer release()
	a, err := sess.Harness().Decide(ctx, req.Msg.GetApprovalSeq(), req.Msg.GetAllow(), req.Msg.GetBy())
	switch {
	case errors.Is(err, durable.ErrApprovalDecided):
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("approval already decided differently"))
	case errors.Is(err, sql.ErrNoRows):
		return nil, connect.NewError(connect.CodeNotFound, errors.New("unknown approval"))
	case errors.Is(err, durable.ErrLeaseLost):
		return nil, leaseLost(sess, err)
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
		info := &v1.SessionInfo{Id: m.ID, Cwd: m.CWD, Name: m.Name, CreatedAt: timestamppb.New(m.CreatedAt), Workspace: workspaceOf(m), Repo: m.Repo, Branch: m.Branch}
		st, err := durable.ReadStatus(ctx, filepath.Join(s.home, ".durable", "sessions", m.ID, "session.sqlite"))
		if err != nil {
			// The warning is returned to the client, so it isn't logged here.
			out.Warnings = append(out.Warnings, fmt.Sprintf("status unavailable for %s", m.ID))
		} else {
			info.PendingApprovals = int32(st.PendingApprovals) //nolint:gosec // counts of rows in one session
			info.UnfinishedSubmissions = int32(st.Unfinished)  //nolint:gosec // counts of rows in one session
		}
		out.Sessions = append(out.Sessions, info)
	}
	return connect.NewResponse(out), nil
}

// minRequestIDLen keeps a guessable request_id from naming, and so opening, another caller's empty session.
const minRequestIDLen = 16

func (s sessionService) CreateSession(ctx context.Context, req *connect.Request[v1.CreateSessionRequest]) (*connect.Response[v1.CreateSessionResponse], error) {
	name := req.Msg.GetName()
	var spec createSpec
	switch req.Msg.GetWorkspace() {
	case v1.Workspace_WORKSPACE_UNSPECIFIED, v1.Workspace_WORKSPACE_DIRECTORY:
		if s.reg.Sandboxed() {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this server creates empty and repo sessions: start it without --sandbox-image for directory sessions"))
		}
		cwd := req.Msg.GetCwd()
		if !filepath.IsAbs(cwd) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cwd must be an absolute path"))
		}
		// Symlinks stay unresolved: the CLI hashes os.Getwd() as given, so both name the same session.
		cwd = filepath.Clean(cwd)
		if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cwd must be an existing directory"))
		}
		// "default" is part of a directory session's key, shared with durable chat.
		if name == "" {
			name = "default"
		}
		spec = createSpec{cwd: cwd, name: name}
	case v1.Workspace_WORKSPACE_EMPTY:
		if req.Msg.GetCwd() != "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("an empty workspace takes no cwd"))
		}
		if req.Msg.GetRequestId() == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("request_id is required for an empty workspace"))
		}
		if len(req.Msg.GetRequestId()) < minRequestIDLen {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("request_id must be at least 16 characters; use a random value"))
		}
		if !s.reg.Sandboxed() {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("empty sessions need a sandbox: start the server with --sandbox-image"))
		}
		spec = createSpec{workspace: commands.WorkspaceEmpty, name: name, id: commands.EmptySessionID(req.Msg.GetRequestId())}
	case v1.Workspace_WORKSPACE_REPO:
		var err error
		if spec, err = s.repoSpec(req.Msg); err != nil {
			return nil, err
		}
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("unknown workspace"))
	}
	meta, created, err := s.reg.Create(ctx, spec)
	if err != nil {
		var (
			se *sandbox.Error
			ce *commands.CloneError
		)
		if errors.Is(err, errCreate) && !errors.As(err, &se) && !errors.As(err, &ce) {
			// The detail can echo a credential reference, so only the log gets it.
			s.logger.ErrorContext(ctx, "request failed", slog.String("op", "create session"), slog.Any("error", err))
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("server can't create sessions: see its log"))
		}
		return nil, s.publicErr(ctx, "create session", err)
	}
	return connect.NewResponse(&v1.CreateSessionResponse{
		Session: &v1.SessionInfo{Id: meta.ID, Cwd: meta.CWD, Name: meta.Name, CreatedAt: timestamppb.New(meta.CreatedAt), Workspace: workspaceOf(meta), Repo: meta.Repo, Branch: meta.Branch},
		Created: created,
	}), nil
}

// repoSpec validates a repo create. Its errors are fixed text, because the input may hold a credential.
func (s sessionService) repoSpec(msg *v1.CreateSessionRequest) (createSpec, error) {
	if !s.reg.Sandboxed() {
		return createSpec{}, connect.NewError(connect.CodeFailedPrecondition, errors.New("repo sessions need a sandbox: start the server with --sandbox-image"))
	}
	if msg.GetCwd() != "" || msg.GetRequestId() != "" {
		return createSpec{}, connect.NewError(connect.CodeInvalidArgument, errors.New("a repo workspace takes no cwd or request_id"))
	}
	repo, err := s.reg.ParseRepo(msg.GetRepo())
	if err != nil {
		return createSpec{}, connect.NewError(connect.CodeInvalidArgument, err)
	}
	branch := msg.GetBranch()
	if branch == "" {
		return createSpec{}, connect.NewError(connect.CodeInvalidArgument, errors.New("branch is required for a repo workspace"))
	}
	if !validBranch(branch) {
		return createSpec{}, connect.NewError(connect.CodeInvalidArgument, errors.New("branch is not a valid branch name"))
	}
	name := msg.GetName()
	if name == "" {
		name = "default"
	}
	return createSpec{
		workspace: commands.WorkspaceRepo, name: name, repo: repo, branch: branch,
		id: commands.RepoSessionID(repo.String(), branch, name),
	}, nil
}

var branchRE = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)

// validBranch accepts a conservative subset of git ref names, so a branch can't be read as an option or a revision expression.
func validBranch(b string) bool {
	return branchRE.MatchString(b) &&
		!strings.HasPrefix(b, "-") && !strings.HasPrefix(b, "/") &&
		!strings.Contains(b, "..") && !strings.Contains(b, "//") && !strings.Contains(b, "@{") &&
		!strings.HasSuffix(b, "/") && !strings.HasSuffix(b, ".") && !strings.HasSuffix(b, ".lock")
}

func workspaceOf(m commands.SessionMeta) v1.Workspace {
	switch m.Workspace {
	case commands.WorkspaceEmpty:
		return v1.Workspace_WORKSPACE_EMPTY
	case commands.WorkspaceRepo:
		return v1.Workspace_WORKSPACE_REPO
	default:
		return v1.Workspace_WORKSPACE_DIRECTORY
	}
}

func (s sessionService) GetServerInfo(context.Context, *connect.Request[v1.GetServerInfoRequest]) (*connect.Response[v1.GetServerInfoResponse], error) {
	kinds := []v1.Workspace{v1.Workspace_WORKSPACE_DIRECTORY}
	if s.reg.Sandboxed() {
		kinds = []v1.Workspace{v1.Workspace_WORKSPACE_EMPTY, v1.Workspace_WORKSPACE_REPO}
	}
	return connect.NewResponse(&v1.GetServerInfoResponse{Workspaces: kinds}), nil
}

// leaseLost marks the session lost and returns Unavailable for an ErrLeaseLost, or nil for any other error.
func leaseLost(sess *Session, err error) error {
	if !errors.Is(err, durable.ErrLeaseLost) {
		return nil
	}
	sess.markLost()
	return errLost()
}

func errLost() error {
	return connect.NewError(connect.CodeUnavailable, errors.New("session taken over by another process"))
}

// internal logs err and returns a code-only error, so internals don't reach the client.
func (s sessionService) internal(ctx context.Context, op string, err error) error {
	s.logger.ErrorContext(ctx, "request failed", slog.String("op", op), slog.Any("error", err))
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

// publicErr keeps codes already chosen and the caller's own cancellation, and hides everything else.
func (s sessionService) publicErr(ctx context.Context, op string, err error) error {
	// First, because an unavailable sandbox can wrap a deadline, which would otherwise pass through with its detail.
	var se *sandbox.Error
	if errors.As(err, &se) && !errors.Is(err, context.Canceled) {
		// Gateway errors can name providers, so only the log gets the cause.
		s.logger.ErrorContext(ctx, "request failed", slog.String("op", op), slog.Any("error", err))
		if se.Unavailable {
			return connect.NewError(connect.CodeUnavailable, errors.New("sandbox unavailable: see the server log"))
		}
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("sandbox can't start: see the server log"))
	}
	if errors.Is(err, commands.ErrEmptyNeedsSandbox) {
		s.logger.ErrorContext(ctx, "request failed", slog.String("op", op), slog.Any("error", err))
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("this session has no sandbox pin: see the server log"))
	}
	// Msg is sanitised git output, safe for the client.
	if cloneErr, ok := errors.AsType[*commands.CloneError](err); ok {
		return connect.NewError(connect.CodeFailedPrecondition, cloneErr)
	}
	var ce *connect.Error
	if errors.As(err, &ce) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return s.internal(ctx, op, err)
}
