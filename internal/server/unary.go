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

type pinnedSession struct {
	info *v1.SessionInfo
	name string
}

func sessionInfo(m commands.SessionMeta) *v1.SessionInfo {
	return &v1.SessionInfo{
		Id:          m.ID,
		Cwd:         m.CWD,
		Name:        m.Name,
		CreatedAt:   timestamppb.New(m.CreatedAt),
		Workspace:   workspaceOf(m),
		Repo:        m.Repo,
		Branch:      m.Branch,
		Environment: m.Environment,
	}
}

func (s sessionService) ListSessions(ctx context.Context, req *connect.Request[v1.ListSessionsRequest]) (*connect.Response[v1.ListSessionsResponse], error) {
	metas, warnings, err := commands.ListSessions(s.home)
	if err != nil {
		return nil, s.internal(ctx, "list sessions", err)
	}
	out := &v1.ListSessionsResponse{Warnings: warnings}
	var pinned []pinnedSession
	for _, m := range metas {
		dir := filepath.Join(s.home, ".durable", "sessions", m.ID)
		info := sessionInfo(m)
		st, err := durable.ReadStatus(ctx, filepath.Join(dir, "session.sqlite"))
		if err != nil {
			// The warning is returned to the client, so it isn't logged here.
			out.Warnings = append(out.Warnings, fmt.Sprintf("status unavailable for %s", m.ID))
		} else {
			info.PendingApprovals = int32(st.PendingApprovals) //nolint:gosec // counts of rows in one session
			info.UnfinishedSubmissions = int32(st.Unfinished)  //nolint:gosec // counts of rows in one session
			info.LastSubmissionStatus = wire.SubmissionStatus(st.LastSubmissionStatus)
		}
		pin, ok, err := sandbox.ReadPin(dir)
		if err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("sandbox status unavailable for %s", m.ID))
		} else if ok {
			pinned = append(pinned, pinnedSession{info: info, name: pin.Name})
		}
		out.Sessions = append(out.Sessions, info)
	}
	s.attachSandboxStatus(out, pinned, req.Msg.GetRefreshSandboxStatus())
	return connect.NewResponse(out), nil
}

func (s sessionService) attachSandboxStatus(out *v1.ListSessionsResponse, pinned []pinnedSession, refresh bool) {
	if len(pinned) == 0 {
		return
	}
	if s.sandboxCache == nil {
		for _, p := range pinned {
			p.info.SandboxStatus = v1.SandboxStatus_SANDBOX_STATUS_UNKNOWN
		}
		return
	}
	snap := s.sandboxCache.Snapshot(refresh)
	for _, p := range pinned {
		p.info.SandboxStatus = sandboxStatusOf(snap, p.name)
	}
	out.SandboxSnapshot = protoSandboxSnapshot(snap)
}

func sandboxStatusOf(snap sandboxSnapshot, name string) v1.SandboxStatus {
	if snap.observedAt.IsZero() {
		return v1.SandboxStatus_SANDBOX_STATUS_UNKNOWN
	}
	phase, ok := snap.phases[name]
	if !ok {
		return v1.SandboxStatus_SANDBOX_STATUS_MISSING
	}
	switch phase {
	case sandbox.PhaseRunning:
		return v1.SandboxStatus_SANDBOX_STATUS_RUNNING
	case sandbox.PhaseStarting:
		return v1.SandboxStatus_SANDBOX_STATUS_STARTING
	case sandbox.PhaseStopping:
		return v1.SandboxStatus_SANDBOX_STATUS_STOPPING
	case sandbox.PhaseStopped:
		return v1.SandboxStatus_SANDBOX_STATUS_STOPPED
	case sandbox.PhaseError:
		return v1.SandboxStatus_SANDBOX_STATUS_ERROR
	default:
		return v1.SandboxStatus_SANDBOX_STATUS_UNKNOWN
	}
}

func protoSandboxSnapshot(snap sandboxSnapshot) *v1.SandboxSnapshot {
	out := &v1.SandboxSnapshot{
		ObservedAt: optTime(snap.observedAt),
		Refreshing: snap.refreshing,
		Stale:      snap.stale,
	}
	if snap.err != nil {
		if snap.observedAt.IsZero() {
			out.Error = "sandbox status unavailable: see the server log"
		} else {
			out.Error = "sandbox status could not be refreshed: see the server log"
		}
	}
	return out
}

// minRequestIDLen keeps a guessable request_id from naming, and so opening, another caller's empty session.
const minRequestIDLen = 16

func (s sessionService) CreateSession(ctx context.Context, req *connect.Request[v1.CreateSessionRequest]) (*connect.Response[v1.CreateSessionResponse], error) {
	name := req.Msg.GetName()
	if err := s.checkEnvironmentApplies(req.Msg); err != nil {
		return nil, err
	}
	var (
		spec createSpec
		err  error
	)
	switch req.Msg.GetWorkspace() {
	case v1.Workspace_WORKSPACE_UNSPECIFIED, v1.Workspace_WORKSPACE_DIRECTORY:
		spec, err = s.directorySpec(req.Msg, name)
	case v1.Workspace_WORKSPACE_EMPTY:
		spec, err = s.emptySpec(req.Msg, name)
	case v1.Workspace_WORKSPACE_REPO:
		spec, err = s.repoSpec(req.Msg)
	default:
		err = connect.NewError(connect.CodeInvalidArgument, errors.New("unknown workspace"))
	}
	if err != nil {
		return nil, err
	}
	meta, created, err := s.reg.Create(ctx, spec)
	if err != nil {
		return nil, s.publicErr(ctx, "create session", err)
	}
	return connect.NewResponse(&v1.CreateSessionResponse{Session: sessionInfo(meta), Created: created}), nil
}

func (s sessionService) directorySpec(msg *v1.CreateSessionRequest, name string) (createSpec, error) {
	if s.reg.Sandboxed() {
		return createSpec{}, connect.NewError(connect.CodeFailedPrecondition, errors.New("this server creates empty and repo sessions: start it without --sandbox-image or environments for directory sessions"))
	}
	cwd := msg.GetCwd()
	if !filepath.IsAbs(cwd) {
		return createSpec{}, connect.NewError(connect.CodeInvalidArgument, errors.New("cwd must be an absolute path"))
	}
	// Symlinks stay unresolved: the CLI hashes os.Getwd() as given, so both name the same session.
	cwd = filepath.Clean(cwd)
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return createSpec{}, connect.NewError(connect.CodeInvalidArgument, errors.New("cwd must be an existing directory"))
	}
	// "default" is part of a directory session's key, shared with durable chat.
	if name == "" {
		name = "default"
	}
	return createSpec{cwd: cwd, name: name}, nil
}

// emptySpec validates an empty create.
func (s sessionService) emptySpec(msg *v1.CreateSessionRequest, name string) (createSpec, error) {
	if msg.GetCwd() != "" {
		return createSpec{}, connect.NewError(connect.CodeInvalidArgument, errors.New("an empty workspace takes no cwd"))
	}
	if msg.GetRequestId() == "" {
		return createSpec{}, connect.NewError(connect.CodeInvalidArgument, errors.New("request_id is required for an empty workspace"))
	}
	if len(msg.GetRequestId()) < minRequestIDLen {
		return createSpec{}, connect.NewError(connect.CodeInvalidArgument, errors.New("request_id must be at least 16 characters; use a random value"))
	}
	if !s.reg.Sandboxed() {
		return createSpec{}, connect.NewError(connect.CodeFailedPrecondition, errors.New("empty sessions need a sandbox: start the server with --sandbox-image or an environment in ~/.durable/config.json or config.yaml"))
	}
	id := commands.EmptySessionID(msg.GetRequestId())
	env, err := s.environment(id, msg.GetEnvironment())
	if err != nil {
		return createSpec{}, err
	}
	return createSpec{workspace: commands.WorkspaceEmpty, name: name, id: id, environment: env}, nil
}

// repoSpec validates a repo create. Its errors are fixed text, because the input may hold a credential.
func (s sessionService) repoSpec(msg *v1.CreateSessionRequest) (createSpec, error) {
	if !s.reg.Sandboxed() {
		return createSpec{}, connect.NewError(connect.CodeFailedPrecondition, errors.New("repo sessions need a sandbox: start the server with --sandbox-image or an environment in ~/.durable/config.json or config.yaml"))
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
	id := commands.RepoSessionID(repo.String(), branch, name)
	env, err := s.environment(id, msg.GetEnvironment())
	if err != nil {
		return createSpec{}, err
	}
	return createSpec{workspace: commands.WorkspaceRepo, name: name, repo: repo, branch: branch, environment: env, id: id}, nil
}

// checkEnvironmentApplies rejects an environment on a directory workspace or a server without a sandbox.
func (s sessionService) checkEnvironmentApplies(msg *v1.CreateSessionRequest) error {
	ws := msg.GetWorkspace()
	directory := ws == v1.Workspace_WORKSPACE_UNSPECIFIED || ws == v1.Workspace_WORKSPACE_DIRECTORY
	if msg.GetEnvironment() != "" && (directory || !s.reg.Sandboxed()) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("an environment applies to empty and repo workspaces on a sandboxed server"))
	}
	return nil
}

// environment resolves the requested environment of an empty or repo create, or returns INVALID_ARGUMENT naming the available ones.
func (s sessionService) environment(id, name string) (string, error) {
	env, err := s.reg.ResolveEnvironment(id, name)
	if err != nil {
		return "", connect.NewError(connect.CodeInvalidArgument, err)
	}
	return env, nil
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
	var envs []*v1.EnvironmentInfo
	for _, e := range s.reg.Environments() {
		envs = append(envs, &v1.EnvironmentInfo{Name: e.Name, Image: e.Image, Grants: e.Grants, Approvals: e.Approvals})
	}
	var settings []*v1.Setting
	for _, st := range s.settings {
		settings = append(settings, &v1.Setting{Field: st.Field, Value: st.Value, Source: st.Source})
	}
	var grants []*v1.GrantInfo
	for _, g := range commands.GrantCatalog() {
		grants = append(grants, &v1.GrantInfo{Name: g.Name, Summary: g.Summary, Binaries: g.Binaries, Hosts: g.Hosts})
	}
	return connect.NewResponse(&v1.GetServerInfoResponse{
		Workspaces: kinds, Environments: envs, Settings: settings, Warnings: s.warnings, Grants: grants,
	}), nil
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

func (s sessionService) logErr(ctx context.Context, op string, err error, code connect.Code, msg string) error {
	s.logger.ErrorContext(ctx, "request failed", slog.String("op", op), slog.Any("error", err))
	return connect.NewError(code, errors.New(msg))
}

// internal logs err and returns a code-only error, so internals don't reach the client.
func (s sessionService) internal(ctx context.Context, op string, err error) error {
	return s.logErr(ctx, op, err, connect.CodeInternal, "internal error")
}

// publicErr keeps codes already chosen and the caller's own cancellation, and hides everything else.
func (s sessionService) publicErr(ctx context.Context, op string, err error) error {
	// First, because an unavailable sandbox can wrap a deadline, which would otherwise pass through with its detail.
	if se, ok := errors.AsType[*sandbox.Error](err); ok {
		if errors.Is(err, context.Canceled) {
			return err
		}
		// Gateway errors can name providers, so only the log gets the cause.
		if se.Unavailable {
			return s.logErr(ctx, op, err, connect.CodeUnavailable, "sandbox unavailable: see the server log")
		}
		return s.logErr(ctx, op, err, connect.CodeFailedPrecondition, "sandbox can't start: see the server log")
	}
	// Msg is sanitised git output, safe for the client.
	if cloneErr, ok := errors.AsType[*commands.CloneError](err); ok {
		return connect.NewError(connect.CodeFailedPrecondition, cloneErr)
	}
	if errors.Is(err, errCreate) {
		// The detail can echo a credential reference, so only the log gets it.
		return s.logErr(ctx, op, err, connect.CodeFailedPrecondition, "server can't create sessions: see its log")
	}
	if errors.Is(err, commands.ErrEmptyNeedsSandbox) {
		return s.logErr(ctx, op, err, connect.CodeFailedPrecondition, "this session has no sandbox pin: see the server log")
	}
	var ce *connect.Error
	if errors.As(err, &ce) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return s.internal(ctx, op, err)
}
