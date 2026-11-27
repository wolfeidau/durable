package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	v1 "github.com/wolfeidau/durable/api/durable/v1"
)

func TestListSessionsShowsApprovalPendingInAnIdleSession(t *testing.T) {
	t.Parallel()
	ws, home := newWorkspace(t)
	id := createSession(t, ws, home, "s")
	_, dbPath := sessionDB(t, home)
	api := newFakeAPI(t, bashReply("echo ran"), 0)
	r := startEnv(t, home, apiEnv(api), "--api-token", "dummy", "--idle-timeout", "1s")
	c := newClient(r)

	_, err := c.submit(t, id, "run it")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return api.count() == 1 }, testTimeout, 20*time.Millisecond)

	// Closed means a probe can take the lease, which the server drops once the parked session idles.
	require.Eventually(t, func() bool {
		probe, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()
		h, err := durable.Open(probe, dbPath, durable.Options{Model: idleModel{}})
		if err != nil {
			return false
		}
		_ = h.Close()
		return true
	}, 30*time.Second, 100*time.Millisecond, "parked session never idled out")

	resp, err := c.ListSessions(t.Context(), authed(c, &v1.ListSessionsRequest{}))
	require.NoError(t, err)
	require.Empty(t, resp.Msg.GetWarnings())
	require.Len(t, resp.Msg.GetSessions(), 1)
	require.EqualValues(t, 1, resp.Msg.GetSessions()[0].GetPendingApprovals())
	require.EqualValues(t, 1, resp.Msg.GetSessions()[0].GetUnfinishedSubmissions())
}

func TestCancelByRequestIDFromAClientThatDidNotSubmit(t *testing.T) {
	t.Parallel()
	ws, home := newWorkspace(t)
	id := createSession(t, ws, home, "s")
	api := newFakeAPI(t, bashReply("echo ran"), 0)
	r := startEnv(t, home, apiEnv(api), "--api-token", "dummy")
	a, b := newClient(r), newClient(r)

	_, err := a.submit(t, id, "run it")
	require.NoError(t, err)

	w := newWatcher(r, id)
	// Stop at the queued event itself, so a missing request_id fails here rather than at the timeout.
	var queued *v1.Event
	w.follow(t, func() bool {
		for _, e := range w.events {
			if e.GetKind() == v1.EventKind_EVENT_KIND_SUBMISSION && e.GetStatus() == v1.SubmissionStatus_SUBMISSION_STATUS_QUEUED {
				queued = e
			}
		}
		return queued != nil
	})
	requestID := queued.GetRequestId()
	require.Equal(t, "req-1", requestID)

	resp, err := b.Cancel(t.Context(), authed(b, &v1.CancelRequest{SessionId: id, RequestId: requestID}))
	require.NoError(t, err)
	require.Equal(t, v1.SubmissionStatus_SUBMISSION_STATUS_CANCELLED, resp.Msg.GetSubmission().GetStatus())
}

func TestListSessionsReportsLastSubmissionStatus(t *testing.T) {
	t.Parallel()

	listOne := func(t *testing.T, c apiClient, id string) *v1.SessionInfo {
		t.Helper()
		resp, err := c.ListSessions(t.Context(), authed(c, &v1.ListSessionsRequest{}))
		require.NoError(t, err)
		require.Empty(t, resp.Msg.GetWarnings())
		for _, s := range resp.Msg.GetSessions() {
			if s.GetId() == id {
				return s
			}
		}
		t.Fatalf("session %s not listed", id)
		return nil
	}

	t.Run("new then done then cancelled", func(t *testing.T) {
		t.Parallel()
		ws, home := newWorkspace(t)
		id := createSession(t, ws, home, "s")
		api := newFakeAPI(t, textReply("finished"), 0)
		r := startEnv(t, home, apiEnv(api), "--api-token", "dummy")
		c := newClient(r)

		// New session has no submissions yet.
		info := listOne(t, c, id)
		require.Equal(t, v1.SubmissionStatus_SUBMISSION_STATUS_UNSPECIFIED, info.GetLastSubmissionStatus())

		// Completing a submission sets DONE.
		_, err := c.submit(t, id, "say hi")
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			s := listOne(t, c, id)
			return s.GetUnfinishedSubmissions() == 0 && s.GetLastSubmissionStatus() == v1.SubmissionStatus_SUBMISSION_STATUS_DONE
		}, testTimeout, 20*time.Millisecond)

		// Cancelling a parked submission sets CANCELLED.
		ws2, home2 := newWorkspace(t)
		id2 := createSession(t, ws2, home2, "s")
		api2 := newFakeAPI(t, bashReply("echo ran"), 0)
		r2 := startEnv(t, home2, apiEnv(api2), "--api-token", "dummy")
		c2 := newClient(r2)
		sub, err := c2.submit(t, id2, "run it")
		require.NoError(t, err)
		require.Eventually(t, func() bool { return api2.count() == 1 }, testTimeout, 20*time.Millisecond)
		_, err = c2.Cancel(t.Context(), authed(c2, &v1.CancelRequest{SessionId: id2, RequestId: sub.GetRequestId()}))
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			s := listOne(t, c2, id2)
			return s.GetUnfinishedSubmissions() == 0 && s.GetLastSubmissionStatus() == v1.SubmissionStatus_SUBMISSION_STATUS_CANCELLED
		}, testTimeout, 20*time.Millisecond)
	})

	t.Run("failed", func(t *testing.T) {
		t.Parallel()
		ws, home := newWorkspace(t)
		id := createSession(t, ws, home, "s")
		errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad prompt"}}`)
		}))
		t.Cleanup(errSrv.Close)
		env := []string{"ANTHROPIC_BASE_URL=" + errSrv.URL, "ANTHROPIC_API_KEY=dummy", "ANTHROPIC_AUTH_TOKEN="}
		r := startEnv(t, home, env, "--api-token", "dummy")
		c := newClient(r)

		_, err := c.submit(t, id, "fail it")
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			s := listOne(t, c, id)
			return s.GetUnfinishedSubmissions() == 0 && s.GetLastSubmissionStatus() == v1.SubmissionStatus_SUBMISSION_STATUS_FAILED
		}, testTimeout, 20*time.Millisecond)
	})
}
