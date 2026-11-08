package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
)

func (c apiClient) create(t *testing.T, cwd, name string) (*v1.CreateSessionResponse, error) {
	t.Helper()
	resp, err := c.CreateSession(t.Context(), authed(c, &v1.CreateSessionRequest{Cwd: cwd, Name: name}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func waitDone(t *testing.T, c apiClient, id string) *v1.Submission {
	t.Helper()
	require.Eventually(t, func() bool {
		s, err := c.submit(t, id, "hi")
		return err == nil && s.GetStatus() == v1.SubmissionStatus_SUBMISSION_STATUS_DONE
	}, testTimeout, 20*time.Millisecond)
	s, err := c.submit(t, id, "hi")
	require.NoError(t, err)
	return s
}

func TestCreateSessionRunsAndIsIdempotent(t *testing.T) {
	ws, home := newWorkspace(t)
	api := newFakeAPI(t, textReply("DONE"), 0)
	c := newClient(startEnv(t, home, apiEnv(api), "--api-token", "dummy"))

	got, err := c.create(t, ws, "")
	require.NoError(t, err)
	require.True(t, got.GetCreated())
	require.Equal(t, ws, got.GetSession().GetCwd())
	require.Equal(t, "default", got.GetSession().GetName())
	require.False(t, got.GetSession().GetCreatedAt().AsTime().IsZero())

	list, err := c.ListSessions(t.Context(), authed(c, &v1.ListSessionsRequest{}))
	require.NoError(t, err)
	require.Len(t, list.Msg.GetSessions(), 1)
	require.Equal(t, got.GetSession().GetId(), list.Msg.GetSessions()[0].GetId())

	require.Equal(t, "DONE", waitDone(t, c, got.GetSession().GetId()).GetResult())

	// An unclean path names the same session as the CLI would.
	again, err := c.create(t, filepath.Join(ws, "."), "default")
	require.NoError(t, err)
	require.False(t, again.GetCreated())
	require.Equal(t, got.GetSession().GetId(), again.GetSession().GetId())
	require.Equal(t, got.GetSession().GetCreatedAt().AsTime(), again.GetSession().GetCreatedAt().AsTime())
}

func TestCreateSessionRejectsBadCWD(t *testing.T) {
	ws, home := newWorkspace(t)
	file := filepath.Join(ws, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	c := newClient(startIn(t, home))

	for name, cwd := range map[string]string{
		"empty":    "",
		"relative": "some/dir",
		"missing":  filepath.Join(ws, "missing"),
		"file":     file,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.create(t, cwd, "")
			require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "%v", err)
		})
	}
}

func TestCreateSessionWithMisconfiguredProviderFailsGenerically(t *testing.T) {
	ws, home := newWorkspace(t)
	api := newFakeAPI(t, textReply("DONE"), 0)
	bad := startEnv(t, home, apiEnv(api), "--provider", "openrouter", "--api-token", "dummy")
	_, err := newClient(bad).create(t, ws, "")
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	require.EqualError(t, err, "failed_precondition: server can't create sessions: see its log")
	pins, err := filepath.Glob(filepath.Join(home, ".durable", "sessions", "*", "provider.json"))
	require.NoError(t, err)
	require.Empty(t, pins)
	bad.stop(t)
	require.Contains(t, bad.out.String(), "create session")

	c := newClient(startEnv(t, home, apiEnv(api), "--api-token", "dummy"))
	got, err := c.create(t, ws, "")
	require.NoError(t, err)
	require.True(t, got.GetCreated())
}

func TestServeProviderFlagsDoNotApplyToExistingSessions(t *testing.T) {
	ws, home := newWorkspace(t)
	api := newFakeAPI(t, textReply("DONE"), 0)
	first := startEnv(t, home, apiEnv(api), "--api-token", "dummy")
	got, err := newClient(first).create(t, ws, "")
	require.NoError(t, err)
	first.stop(t)

	c := newClient(startEnv(t, home, apiEnv(api), "--api-token", "dummy", "--provider", "openrouter", "--model", "x"))
	require.Equal(t, "DONE", waitDone(t, c, got.GetSession().GetId()).GetResult())
}
