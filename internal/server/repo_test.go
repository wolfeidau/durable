package server

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/internal/commands"
	"github.com/wolfeidau/durable/internal/sandbox"
)

func (e *sandboxEnv) createRepo(t *testing.T, repo, branch, name string) (*connect.Response[v1.CreateSessionResponse], error) {
	t.Helper()
	return e.create(t, &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_REPO, Repo: repo, Branch: branch, Name: name})
}

func TestRepoCreateClonesAndIsIdempotent(t *testing.T) {
	e := newSandboxEnv(t)

	first, err := e.createRepo(t, "github.com/o/n", "feature", "")
	require.NoError(t, err)
	require.True(t, first.Msg.Created)
	s := first.Msg.Session
	require.Equal(t, v1.Workspace_WORKSPACE_REPO, s.Workspace)
	require.Equal(t, "github.com/o/n", s.Repo)
	require.Equal(t, "feature", s.Branch)
	require.Equal(t, "default", s.Name)
	require.Empty(t, s.Cwd)
	require.Equal(t, commands.RepoSessionID("github.com/o/n", "feature", "default"), s.Id)

	again, err := e.createRepo(t, "https://github.com/O/N.git", "feature", "default")
	require.NoError(t, err)
	require.False(t, again.Msg.Created)
	require.Equal(t, s.Id, again.Msg.Session.Id)

	other, err := e.createRepo(t, "github.com/o/n", "main", "")
	require.NoError(t, err)
	require.True(t, other.Msg.Created)
	require.NotEqual(t, s.Id, other.Msg.Session.Id)
	named, err := e.createRepo(t, "github.com/o/n", "feature", "second")
	require.NoError(t, err)
	require.NotEqual(t, s.Id, named.Msg.Session.Id)

	listed, err := e.client.ListSessions(t.Context(), authed(&v1.ListSessionsRequest{}))
	require.NoError(t, err)
	var found *v1.SessionInfo
	for _, li := range listed.Msg.Sessions {
		if li.Id == s.Id {
			found = li
		}
	}
	require.NotNil(t, found)
	require.Equal(t, v1.Workspace_WORKSPACE_REPO, found.Workspace)
	require.Equal(t, "github.com/o/n", found.Repo)
	require.Equal(t, "feature", found.Branch)

	sess, release, err := e.reg.Acquire(t.Context(), s.Id)
	require.NoError(t, err)
	defer release()
	require.NotNil(t, sess)
	require.FileExists(t, filepath.Join(e.fake.Root(pinName(t, e.home, s.Id)), "feature.txt"))
}

func TestRepoCreateRejectsBadRequests(t *testing.T) {
	e := newSandboxEnv(t)
	tests := []struct {
		name string
		msg  *v1.CreateSessionRequest
		code connect.Code
		bad  string
	}{
		{"cwd", &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_REPO, Repo: "github.com/o/n", Branch: "main", Cwd: "/CWDMARK"}, connect.CodeInvalidArgument, "CWDMARK"},
		{"request id", &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_REPO, Repo: "github.com/o/n", Branch: "main", RequestId: "REQMARK0123456789"}, connect.CodeInvalidArgument, "REQMARK"},
		{"no repo", &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_REPO, Branch: "main"}, connect.CodeInvalidArgument, ""},
		{"unsupported host", &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_REPO, Repo: "https://gitlab.example/o/HOSTMARK", Branch: "main"}, connect.CodeInvalidArgument, "HOSTMARK"},
		{"credential in the url", &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_REPO, Repo: "https://x:SECRETMARK@github.com/o/n", Branch: "main"}, connect.CodeInvalidArgument, "SECRETMARK"},
		{"no branch", &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_REPO, Repo: "github.com/o/n"}, connect.CodeInvalidArgument, ""},
		{"leading dash", &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_REPO, Repo: "github.com/o/n", Branch: "-DASHMARK"}, connect.CodeInvalidArgument, "DASHMARK"},
		{"leading slash", &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_REPO, Repo: "github.com/o/n", Branch: "/SLASHMARK"}, connect.CodeInvalidArgument, "SLASHMARK"},
		{"dot dot", &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_REPO, Repo: "github.com/o/n", Branch: "a..DOTMARK"}, connect.CodeInvalidArgument, "DOTMARK"},
		{"double slash", &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_REPO, Repo: "github.com/o/n", Branch: "a//DSMARK"}, connect.CodeInvalidArgument, "DSMARK"},
		{"reflog syntax", &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_REPO, Repo: "github.com/o/n", Branch: "a@{ATMARK"}, connect.CodeInvalidArgument, "ATMARK"},
		{"trailing slash", &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_REPO, Repo: "github.com/o/n", Branch: "TSMARK/"}, connect.CodeInvalidArgument, "TSMARK"},
		{"trailing dot", &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_REPO, Repo: "github.com/o/n", Branch: "TDMARK."}, connect.CodeInvalidArgument, "TDMARK"},
		{"lock suffix", &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_REPO, Repo: "github.com/o/n", Branch: "LOCKMARK.lock"}, connect.CodeInvalidArgument, "LOCKMARK"},
		{"space", &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_REPO, Repo: "github.com/o/n", Branch: "SPMARK x"}, connect.CodeInvalidArgument, "SPMARK"},
		{"too long", &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_REPO, Repo: "github.com/o/n", Branch: "LONGMARK" + strings.Repeat("a", 200)}, connect.CodeInvalidArgument, "LONGMARK"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.create(t, tc.msg)
			require.Equal(t, tc.code, connect.CodeOf(err))
			if tc.bad != "" {
				require.NotContains(t, err.Error(), tc.bad)
			}
		})
	}
	ensures, _, _ := e.fake.Counts()
	require.Zero(t, ensures)
}

func TestRepoCreateCloneFailureIsFailedPrecondition(t *testing.T) {
	e := newSandboxEnv(t)

	_, err := e.createRepo(t, "github.com/o/n", "no-such-branch", "")

	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	require.Contains(t, err.Error(), "failed to clone the repository: ")
	require.Contains(t, err.Error(), "no-such-branch")
	_, _, deletes := e.fake.Counts()
	require.Equal(t, 1, deletes)
}

func authed[T any](msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+testToken)
	return req
}

func pinName(t *testing.T, home, id string) string {
	t.Helper()
	pin, ok, err := sandbox.ReadPin(filepath.Join(home, ".durable", "sessions", id))
	require.NoError(t, err)
	require.True(t, ok)
	return pin.Name
}

func TestRepoCreateOnPlainServerIsRefused(t *testing.T) {
	e := newEnv(t)
	r := e.registry(t)
	svc := sessionService{reg: r, home: e.home, logger: slog.New(slog.DiscardHandler)}

	_, err := svc.CreateSession(t.Context(), connect.NewRequest(&v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_REPO, Repo: "github.com/o/n", Branch: "main"}))

	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
}

func TestRepoSessionReopensWithoutGitHosts(t *testing.T) {
	e := newSandboxEnv(t)
	res, err := e.createRepo(t, "github.com/o/n", "feature", "")
	require.NoError(t, err)
	id := res.Msg.Session.Id
	e.reg.Close()

	// A server restarted without --sandbox-image has no git hosts, but the pin wins for an existing session.
	e.cfg.Sandbox = &commands.SandboxConfig{Backend: e.fake}
	r := e.registry(t)
	sess, release, err := r.Acquire(t.Context(), id)
	require.NoError(t, err)
	defer release()
	require.NotNil(t, sess)
}
