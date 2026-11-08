package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	v1 "github.com/wolfeidau/durable/api/durable/v1"
)

func TestListSessionsShowsApprovalPendingInAnIdleSession(t *testing.T) {
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
	ws, home := newWorkspace(t)
	id := createSession(t, ws, home, "s")
	api := newFakeAPI(t, bashReply("echo ran"), 0)
	r := startEnv(t, home, apiEnv(api), "--api-token", "dummy")
	a, b := newClient(r), newClient(r)

	_, err := a.submit(t, id, "run it")
	require.NoError(t, err)

	w := newWatcher(r, id)
	var requestID string
	w.follow(t, func() bool {
		for _, e := range w.events {
			if e.GetKind() == v1.EventKind_EVENT_KIND_SUBMISSION && e.GetStatus() == v1.SubmissionStatus_SUBMISSION_STATUS_QUEUED {
				requestID = e.GetRequestId()
			}
		}
		return requestID != ""
	})
	require.Equal(t, "req-1", requestID)

	resp, err := b.Cancel(t.Context(), authed(b, &v1.CancelRequest{SessionId: id, RequestId: requestID}))
	require.NoError(t, err)
	require.Equal(t, v1.SubmissionStatus_SUBMISSION_STATUS_CANCELLED, resp.Msg.GetSubmission().GetStatus())
}
