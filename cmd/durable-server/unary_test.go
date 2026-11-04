package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/api/durable/v1/durablev1connect"
)

type apiClient struct {
	durablev1connect.SessionServiceClient
	token string
}

func newClient(r *running) apiClient {
	return apiClient{durablev1connect.NewSessionServiceClient(h2cClient(), r.url), r.token}
}

func authed[T any](c apiClient, msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+c.token)
	return req
}

func (c apiClient) submit(t *testing.T, id, text string) (*v1.Submission, error) {
	t.Helper()
	resp, err := c.Submit(t.Context(), authed(c, &v1.SubmitRequest{SessionId: id, RequestId: "req-1", Text: text}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSubmission(), nil
}

func (c apiClient) decide(t *testing.T, id string, seq int64, allow bool) (*v1.Approval, error) {
	t.Helper()
	resp, err := c.DecideApproval(t.Context(), authed(c, &v1.DecideApprovalRequest{SessionId: id, ApprovalSeq: seq, Allow: allow, By: "test"}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetApproval(), nil
}

func TestSubmitRunsToDoneAndIsExactlyOnce(t *testing.T) {
	ws, home := newWorkspace(t)
	id := createSession(t, ws, home, "s")
	api := newFakeAPI(t, textReply("hello there"), 0)
	c := newClient(startEnv(t, home, apiEnv(api), "--api-token", "dummy"))

	first, err := c.submit(t, id, "hi")
	require.NoError(t, err)
	require.NotZero(t, first.GetSeq())
	again, err := c.submit(t, id, "hi")
	require.NoError(t, err)
	require.Equal(t, first.GetSeq(), again.GetSeq())

	require.Eventually(t, func() bool {
		s, err := c.submit(t, id, "hi")
		return err == nil && s.GetStatus() == v1.SubmissionStatus_SUBMISSION_STATUS_DONE
	}, testTimeout, 20*time.Millisecond)
	s, err := c.submit(t, id, "hi")
	require.NoError(t, err)
	require.Equal(t, "hello there", s.GetResult())
	require.Equal(t, 1, api.count(), "the repeated request must not run again")
}

func TestApprovalRunsExactlyOnce(t *testing.T) {
	ws, home := newWorkspace(t)
	id := createSession(t, ws, home, "s")
	marker := filepath.Join(ws, "marker.txt")
	api := newFakeAPI(t, bashReply("echo ran >> marker.txt"), 0)
	c := newClient(startEnv(t, home, apiEnv(api), "--api-token", "dummy"))

	_, err := c.submit(t, id, "run it")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return api.count() == 1 }, testTimeout, 20*time.Millisecond)
	// Parked: the model has answered with a tool call, and nothing ran without a decision.
	s, err := c.submit(t, id, "run it")
	require.NoError(t, err)
	require.Equal(t, v1.SubmissionStatus_SUBMISSION_STATUS_RUNNING, s.GetStatus())

	// Approval 1 doesn't exist until the run parks, so NotFound means not yet.
	var a *v1.Approval
	var ranBeforeDecision bool
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(marker)
		ranBeforeDecision = ranBeforeDecision || statErr == nil
		a, err = c.decide(t, id, 1, true)
		return connect.CodeOf(err) != connect.CodeNotFound
	}, testTimeout, 20*time.Millisecond)
	require.False(t, ranBeforeDecision, "the call must not run before it is approved")
	require.NoError(t, err)
	require.Equal(t, v1.ApprovalStatus_APPROVAL_STATUS_ALLOWED, a.GetStatus())
	require.Equal(t, "bash", a.GetCall().GetName())

	require.Eventually(t, func() bool {
		s, err := c.submit(t, id, "run it")
		return err == nil && s.GetStatus() == v1.SubmissionStatus_SUBMISSION_STATUS_DONE
	}, testTimeout, 20*time.Millisecond)
	require.Len(t, readLines(t, marker), 1)

	// The same decision again is a no-op.
	dup, err := c.decide(t, id, 1, true)
	require.NoError(t, err)
	require.Equal(t, v1.ApprovalStatus_APPROVAL_STATUS_ALLOWED, dup.GetStatus())
	// A different one fails.
	_, err = c.decide(t, id, 1, false)
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	_, err = c.decide(t, id, 99, true)
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err))

	time.Sleep(300 * time.Millisecond)
	require.Len(t, readLines(t, marker), 1, "a repeated decision must not run the call again")
	require.Equal(t, 2, api.count())
}

func TestCancelParkedSubmission(t *testing.T) {
	ws, home := newWorkspace(t)
	id := createSession(t, ws, home, "s")
	marker := filepath.Join(ws, "marker.txt")
	api := newFakeAPI(t, bashReply("echo ran >> marker.txt"), 0)
	c := newClient(startEnv(t, home, apiEnv(api), "--api-token", "dummy"))

	_, err := c.submit(t, id, "run it")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return api.count() == 1 }, testTimeout, 20*time.Millisecond)

	// Cancel waits for the run to park, then cancels.
	resp, err := c.Cancel(t.Context(), authed(c, &v1.CancelRequest{SessionId: id, RequestId: "req-1"}))
	require.NoError(t, err)
	require.Equal(t, v1.SubmissionStatus_SUBMISSION_STATUS_CANCELLED, resp.Msg.GetSubmission().GetStatus())
	_, err = os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist)

	_, err = c.Cancel(t.Context(), authed(c, &v1.CancelRequest{SessionId: id, RequestId: "nope"}))
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
}

func TestListSessions(t *testing.T) {
	ws, home := newWorkspace(t)
	a := createSession(t, ws, home, "a")
	b := createSession(t, ws, home, "b")
	bad := filepath.Join(home, ".durable", "sessions", "00000000000000ff")
	require.NoError(t, os.MkdirAll(bad, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(bad, "session.json"), []byte("{"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(bad, "session.sqlite"), nil, 0o600))
	c := newClient(startIn(t, home))

	resp, err := c.ListSessions(t.Context(), authed(c, &v1.ListSessionsRequest{}))
	require.NoError(t, err)
	got := map[string]*v1.SessionInfo{}
	for _, s := range resp.Msg.GetSessions() {
		got[s.GetId()] = s
	}
	require.Len(t, got, 2)
	require.Equal(t, "a", got[a].GetName())
	require.Equal(t, "b", got[b].GetName())
	require.Equal(t, ws, got[a].GetCwd())
	require.False(t, got[a].GetCreatedAt().AsTime().IsZero())
	require.Len(t, resp.Msg.GetWarnings(), 1)
	require.Contains(t, resp.Msg.GetWarnings()[0], "malformed")
}

func TestUnknownAndMalformedSessionIDs(t *testing.T) {
	_, home := newWorkspace(t)
	c := newClient(startIn(t, home, "--api-token", "dummy"))

	_, err := c.submit(t, "0123456789abcdef", "hi")
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
	_, err = c.submit(t, "../../etc", "hi")
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	_, err = c.decide(t, "../x", 1, true)
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	_, err = c.Cancel(t.Context(), authed(c, &v1.CancelRequest{SessionId: "", RequestId: "r"}))
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
}

func TestCancelInterruptsRunningSubmission(t *testing.T) {
	ws, home := newWorkspace(t)
	id := createSession(t, ws, home, "s")
	api := newHoldingAPI(t)
	c := newClient(startEnv(t, home, apiEnv(api), "--api-token", "dummy"))

	_, err := c.submit(t, id, "run it")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return api.count() == 1 }, testTimeout, 20*time.Millisecond)

	// The model request is held for the whole test timeout, so only an interrupt can cancel this quickly.
	start := time.Now()
	resp, err := c.Cancel(t.Context(), authed(c, &v1.CancelRequest{SessionId: id, RequestId: "req-1"}))
	require.NoError(t, err)
	require.Equal(t, v1.SubmissionStatus_SUBMISSION_STATUS_CANCELLED, resp.Msg.GetSubmission().GetStatus())
	require.Less(t, time.Since(start), 15*time.Second)

	submit2 := func() (*v1.Submission, error) {
		r, err := c.Submit(t.Context(), authed(c, &v1.SubmitRequest{SessionId: id, RequestId: "req-2", Text: "again"}))
		if err != nil {
			return nil, err
		}
		return r.Msg.GetSubmission(), nil
	}
	_, err = submit2()
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		s, err := submit2()
		return err == nil && s.GetStatus() == v1.SubmissionStatus_SUBMISSION_STATUS_DONE
	}, 30*time.Second, 20*time.Millisecond)
}

func TestOpenFailureIsInternalWithoutPaths(t *testing.T) {
	ws, home := newWorkspace(t)
	id := createSession(t, ws, home, "s")
	meta := filepath.Join(home, ".durable", "sessions", id, "session.json")
	require.NoError(t, os.WriteFile(meta, []byte("{"), 0o600))
	c := newClient(startIn(t, home, "--api-token", "dummy"))

	_, err := c.submit(t, id, "hi")
	require.Equal(t, connect.CodeInternal, connect.CodeOf(err))
	require.NotContains(t, err.Error(), home)
}

func TestSubmitWhileChatHoldsSessionIsUnavailable(t *testing.T) {
	ws, home := newWorkspace(t)
	id := createSession(t, ws, home, "s")
	api := newFakeAPI(t, textReply("from chat"), 0)
	chat := startChat(t, ws, home, "s", api)
	_, err := chat.stdin.Write([]byte("hold\n"))
	require.NoError(t, err)
	// The reply proves chat has the session open; stdin stays open so it keeps the lease.
	require.Eventually(t, func() bool { return api.count() == 1 }, testTimeout, 20*time.Millisecond, chat.out.String())

	c := newClient(startEnv(t, home, apiEnv(api), "--api-token", "dummy", "--open-timeout", "500ms"))
	_, err = c.submit(t, id, "hi")
	require.Equal(t, connect.CodeUnavailable, connect.CodeOf(err))
	require.Contains(t, err.Error(), "session in use")
}
