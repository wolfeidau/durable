package server

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/internal/sandbox/openshell"
	"github.com/wolfeidau/durable/internal/telemetry"
)

func TestListNetworkEventsResumeReturnsLateArrivals(t *testing.T) {
	home := t.TempDir()
	writePin(t, home)
	store := openNetStore(t)
	start := time.Now().Add(-time.Hour)
	seedDecisions(t, store, 3, start)
	svc := netService(home, store, nil)
	first, err := list(t, svc, &v1.ListNetworkEventsRequest{SessionId: netSession})
	require.NoError(t, err)

	// A retried export lands an event older than the ones already returned.
	at := start.Add(-time.Minute)
	_, _, err = store.Add(t.Context(), []telemetry.Event{{
		UID: "late", SandboxName: netSandbox, SandboxID: "id1", Time: at, Observed: at,
		Decision: &openshell.Decision{Time: at, Kind: "HTTP", Host: "late.com"},
	}}, time.Time{})
	require.NoError(t, err)
	next, err := list(t, svc, &v1.ListNetworkEventsRequest{SessionId: netSession, After: first.NextCursor})

	require.NoError(t, err)
	require.Equal(t, []string{"late"}, eventIDs(next))
	require.False(t, next.CursorReset)
}

func TestListNetworkEventsCursorBeyondStoreResets(t *testing.T) {
	home := t.TempDir()
	writePin(t, home)
	big := openNetStore(t)
	seedDecisions(t, big, 5, time.Now().Add(-time.Hour))
	first, err := list(t, netService(home, big, nil), &v1.ListNetworkEventsRequest{SessionId: netSession})
	require.NoError(t, err)
	small := openNetStore(t)
	seedDecisions(t, small, 2, time.Now().Add(-time.Hour))

	res, err := list(t, netService(home, small, nil), &v1.ListNetworkEventsRequest{SessionId: netSession, After: first.NextCursor})

	require.NoError(t, err)
	require.True(t, res.CursorReset)
	require.Equal(t, []string{"u00", "u01"}, eventIDs(res))
}

func TestListNetworkEventsGatewayLogIssuesNoCursor(t *testing.T) {
	home := t.TempDir()
	writePin(t, home)
	at := time.Now().Add(-time.Hour)
	svc := netService(home, openNetStore(t), func(context.Context, string, time.Time) ([]openshell.Decision, error) {
		return []openshell.Decision{{Time: at, Kind: "NET", Host: "x.com:443"}}, nil
	})

	res, err := list(t, svc, &v1.ListNetworkEventsRequest{SessionId: netSession})

	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	require.Empty(t, res.NextCursor)
}

func TestListNetworkEventsRejectsGatewayCursors(t *testing.T) {
	home := t.TempDir()
	writePin(t, home)
	svc := netService(home, openNetStore(t), nil)
	cursor := base64.RawURLEncoding.EncodeToString([]byte("g:1700000000000:g-abc-0"))

	_, err := list(t, svc, &v1.ListNetworkEventsRequest{SessionId: netSession, After: cursor})

	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "%v", err)
}
