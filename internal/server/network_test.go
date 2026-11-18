package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/internal/otlp"
	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/sandbox/openshell"
	"github.com/wolfeidau/durable/internal/telemetry"
)

const (
	netSession = "0123456789abcdef"
	netSandbox = "dt-network-test"
)

func writePin(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, ".durable", "sessions", netSession)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	data, err := json.Marshal(sandbox.Pin{Name: netSandbox, Image: "worker:test", Token: "unused"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, sandbox.PinFile), data, 0o600))
}

func openNetStore(t *testing.T) *telemetry.Store {
	t.Helper()
	s, err := telemetry.Open(t.Context(), filepath.Join(t.TempDir(), "telemetry.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedDecisions(t *testing.T, s *telemetry.Store, n int, start time.Time) {
	t.Helper()
	var evs []telemetry.Event
	for i := range n {
		at := start.Add(time.Duration(i) * time.Second)
		evs = append(evs, telemetry.Event{
			UID: fmt.Sprintf("u%02d", i), SandboxName: netSandbox, SandboxID: "id1", Time: at, Observed: at,
			Decision: &openshell.Decision{Time: at, Allowed: i%2 == 0, Kind: "HTTP", Method: "GET", Host: fmt.Sprintf("h%d.com", i), Path: "/a/v1.2.3/b", Policy: "p", Reason: "r"},
		})
	}
	_, _, err := s.Add(t.Context(), evs, time.Time{})
	require.NoError(t, err)
}

func netService(home string, store *telemetry.Store, gw func(context.Context, string, time.Time) ([]openshell.Decision, error)) sessionService {
	svc := sessionService{home: home, logger: slog.New(slog.DiscardHandler), telemetry: store, retention: 720 * time.Hour}
	if gw != nil {
		svc.gateway = gw
	}
	return svc
}

func list(t *testing.T, svc sessionService, msg *v1.ListNetworkEventsRequest) (*v1.ListNetworkEventsResponse, error) {
	t.Helper()
	res, err := svc.ListNetworkEvents(t.Context(), connect.NewRequest(msg))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func TestListNetworkEventsUnknownSession(t *testing.T) {
	svc := netService(t.TempDir(), nil, nil)

	_, err := list(t, svc, &v1.ListNetworkEventsRequest{SessionId: netSession})

	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err), "%v", err)
}

func TestListNetworkEventsRejectsBadSessionID(t *testing.T) {
	svc := netService(t.TempDir(), nil, nil)

	_, err := list(t, svc, &v1.ListNetworkEventsRequest{SessionId: "../x"})

	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "%v", err)
}

func TestListNetworkEventsNoSandboxPin(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable", "sessions", netSession), 0o700))
	svc := netService(home, nil, nil)

	_, err := list(t, svc, &v1.ListNetworkEventsRequest{SessionId: netSession})

	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err), "%v", err)
	require.Contains(t, err.Error(), "no sandbox")
}

func TestListNetworkEventsFromStore(t *testing.T) {
	home := t.TempDir()
	writePin(t, home)
	store := openNetStore(t)
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	seedDecisions(t, store, 3, start)
	svc := netService(home, store, func(context.Context, string, time.Time) ([]openshell.Decision, error) {
		return nil, errors.New("must not be read")
	})

	res, err := list(t, svc, &v1.ListNetworkEventsRequest{SessionId: netSession})

	require.NoError(t, err)
	require.Len(t, res.Events, 3)
	e := res.Events[0]
	require.Equal(t, "u00", e.Id)
	require.True(t, e.Allowed)
	require.Equal(t, "HTTP", e.Kind)
	require.Equal(t, "h0.com", e.Host)
	require.Equal(t, "/a/v1.2.3/b", e.Path)
	require.Equal(t, "/a/*/b", e.Pattern)
	require.Equal(t, "p", e.Policy)
	require.Equal(t, "r", e.Reason)
	require.True(t, start.Equal(e.Time.AsTime()))
	require.False(t, res.Truncated)
	require.NotEmpty(t, res.NextCursor)
	c := res.Coverage
	require.Equal(t, v1.NetworkSource_NETWORK_SOURCE_STORE, c.Source)
	require.True(t, c.StoreAvailable)
	require.True(t, c.Resumable)
	require.False(t, c.SourceBounded)
	require.Empty(t, c.GatewayError)
	require.Equal(t, 720*time.Hour, c.Retention.AsDuration())
	require.NotNil(t, c.Since)
	require.NotEmpty(t, c.UnattributedFromDay)
	require.NotNil(t, c.HistoryStart, "the earliest stored decision is after the 24h default window start")
}

func TestListNetworkEventsReportsDropsAndUnattributed(t *testing.T) {
	home := t.TempDir()
	writePin(t, home)
	store := openNetStore(t)
	seedDecisions(t, store, 1, time.Now().Add(-time.Hour))
	gapAt := time.Now().Add(-30 * time.Minute)
	_, _, err := store.Add(t.Context(), []telemetry.Event{
		{SandboxID: "id1", Observed: gapAt, Raw: []byte(`{}`), Gap: &otlp.Gap{Dropped: 4, Since: gapAt.Add(-time.Minute)}},
		{SandboxID: "", Observed: gapAt, Raw: []byte(`{}`), Gap: &otlp.Gap{Dropped: 6, Since: gapAt.Add(-time.Minute)}},
		{SandboxID: "never-seen", Observed: gapAt, Raw: []byte(`{}`), Gap: &otlp.Gap{Dropped: 8, Since: gapAt.Add(-time.Minute)}},
		{UID: "", Observed: gapAt, Raw: []byte(`{}`), Decision: &openshell.Decision{Time: gapAt}},
	}, time.Time{})
	require.NoError(t, err)

	res, err := list(t, netService(home, store, nil), &v1.ListNetworkEventsRequest{SessionId: netSession})

	require.NoError(t, err)
	c := res.Coverage
	require.Equal(t, int64(4), c.DroppedOwn.Dropped)
	require.Equal(t, int64(6), c.DroppedGatewayWide.Dropped)
	require.Equal(t, int64(8), c.DroppedUnknown.Dropped)
	require.NotNil(t, c.DroppedOwn.Since)
	require.Equal(t, int64(1), c.Unattributed)
}

func TestListNetworkEventsMarksPrunedWindow(t *testing.T) {
	home := t.TempDir()
	writePin(t, home)
	svc := netService(home, openNetStore(t), func(context.Context, string, time.Time) ([]openshell.Decision, error) { return nil, nil })

	res, err := list(t, svc, &v1.ListNetworkEventsRequest{SessionId: netSession, Since: &v1.ListNetworkEventsRequest_SinceAgo{SinceAgo: durationpb.New(800 * time.Hour)}})

	require.NoError(t, err)
	require.True(t, res.Coverage.Pruned)
}

func TestListNetworkEventsTruncationAndCursorRoundTrip(t *testing.T) {
	home := t.TempDir()
	writePin(t, home)
	store := openNetStore(t)
	seedDecisions(t, store, 5, time.Now().Add(-time.Hour))
	svc := netService(home, store, nil)

	first, err := list(t, svc, &v1.ListNetworkEventsRequest{SessionId: netSession, Limit: 2})
	require.NoError(t, err)
	require.True(t, first.Truncated, "older events were left out")
	require.Equal(t, []string{"u03", "u04"}, eventIDs(first))

	seedMore := func(uid string, at time.Time) {
		_, _, err := store.Add(t.Context(), []telemetry.Event{{
			UID: uid, SandboxName: netSandbox, SandboxID: "id1", Time: at, Observed: at,
			Decision: &openshell.Decision{Time: at, Kind: "HTTP", Host: uid + ".com"},
		}}, time.Time{})
		require.NoError(t, err)
	}
	seedMore("x1", time.Now().Add(-time.Minute))
	seedMore("x2", time.Now().Add(-30*time.Second))
	seedMore("x3", time.Now().Add(-10*time.Second))

	next, err := list(t, svc, &v1.ListNetworkEventsRequest{SessionId: netSession, Limit: 2, After: first.NextCursor})
	require.NoError(t, err)
	require.Equal(t, []string{"x1", "x2"}, eventIDs(next))
	require.True(t, next.Truncated, "newer events remain")

	last, err := list(t, svc, &v1.ListNetworkEventsRequest{SessionId: netSession, Limit: 2, After: next.NextCursor})
	require.NoError(t, err)
	require.Equal(t, []string{"x3"}, eventIDs(last))
	require.False(t, last.Truncated)

	idle, err := list(t, svc, &v1.ListNetworkEventsRequest{SessionId: netSession, After: last.NextCursor})
	require.NoError(t, err)
	require.Empty(t, idle.Events)
	require.Equal(t, last.NextCursor, idle.NextCursor)
	require.Equal(t, v1.NetworkSource_NETWORK_SOURCE_STORE, idle.Coverage.Source)
}

func eventIDs(r *v1.ListNetworkEventsResponse) []string {
	out := make([]string, len(r.Events))
	for i, e := range r.Events {
		out[i] = e.Id
	}
	return out
}

func TestListNetworkEventsFallsBackToGatewayLog(t *testing.T) {
	home := t.TempDir()
	writePin(t, home)
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	svc := netService(home, openNetStore(t), func(_ context.Context, name string, _ time.Time) ([]openshell.Decision, error) {
		require.Equal(t, netSandbox, name)
		return []openshell.Decision{{Time: at, Allowed: false, Kind: "NET", Activity: "REFUSE", Host: "x.com:443"}}, nil
	})

	res, err := list(t, svc, &v1.ListNetworkEventsRequest{SessionId: netSession})

	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	require.NotEmpty(t, res.Events[0].Id)
	c := res.Coverage
	require.Equal(t, v1.NetworkSource_NETWORK_SOURCE_GATEWAY_LOG, c.Source)
	require.True(t, c.SourceBounded)
	require.False(t, c.Resumable)
	require.True(t, c.StoreAvailable)
	require.True(t, c.StoreEmpty)
	require.NotNil(t, c.GatewayLogStart, "the log starts after the 24h window start")
}

func TestListNetworkEventsWithoutStoreSaysSo(t *testing.T) {
	home := t.TempDir()
	writePin(t, home)
	svc := netService(home, nil, func(context.Context, string, time.Time) ([]openshell.Decision, error) {
		return nil, errors.New("dial gateway: secret detail")
	})

	res, err := list(t, svc, &v1.ListNetworkEventsRequest{SessionId: netSession})

	require.NoError(t, err)
	require.Empty(t, res.Events)
	c := res.Coverage
	require.Equal(t, v1.NetworkSource_NETWORK_SOURCE_GATEWAY_LOG, c.Source)
	require.False(t, c.StoreAvailable)
	require.NotEmpty(t, c.GatewayError)
	require.NotContains(t, c.GatewayError, "secret detail")
	require.Nil(t, c.Retention)
}

func TestListNetworkEventsWithoutGatewayReader(t *testing.T) {
	home := t.TempDir()
	writePin(t, home)

	res, err := list(t, netService(home, nil, nil), &v1.ListNetworkEventsRequest{SessionId: netSession})

	require.NoError(t, err)
	require.NotEmpty(t, res.Coverage.GatewayError)
}

func TestListNetworkEventsValidatesRequest(t *testing.T) {
	home := t.TempDir()
	writePin(t, home)
	svc := netService(home, openNetStore(t), nil)

	tests := []struct {
		name string
		req  *v1.ListNetworkEventsRequest
	}{
		{"negative limit", &v1.ListNetworkEventsRequest{Limit: -1}},
		{"limit over max", &v1.ListNetworkEventsRequest{Limit: 1001}},
		{"zero since_ago", &v1.ListNetworkEventsRequest{Since: &v1.ListNetworkEventsRequest_SinceAgo{SinceAgo: durationpb.New(0)}}},
		{"negative since_ago", &v1.ListNetworkEventsRequest{Since: &v1.ListNetworkEventsRequest_SinceAgo{SinceAgo: durationpb.New(-time.Hour)}}},
		{"malformed cursor", &v1.ListNetworkEventsRequest{After: "!!!"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.SessionId = netSession
			_, err := list(t, svc, tc.req)
			require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "%v", err)
		})
	}
}

func TestListNetworkEventsSinceTimeNarrowsWindow(t *testing.T) {
	home := t.TempDir()
	writePin(t, home)
	store := openNetStore(t)
	start := time.Now().Add(-time.Hour)
	seedDecisions(t, store, 4, start)

	res, err := list(t, netService(home, store, nil), &v1.ListNetworkEventsRequest{
		SessionId: netSession,
		Since:     &v1.ListNetworkEventsRequest_SinceTime{SinceTime: timestamppb.New(start.Add(2 * time.Second))},
	})

	require.NoError(t, err)
	require.Equal(t, []string{"u02", "u03"}, eventIDs(res))
}
