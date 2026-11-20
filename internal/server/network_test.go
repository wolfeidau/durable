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
	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/sandbox/openshell"
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

func netService(home string, gw func(context.Context, string, time.Time) ([]openshell.Decision, error)) sessionService {
	svc := sessionService{home: home, logger: slog.New(slog.DiscardHandler)}
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
	svc := netService(t.TempDir(), nil)

	_, err := list(t, svc, &v1.ListNetworkEventsRequest{SessionId: netSession})

	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err), "%v", err)
}

func TestListNetworkEventsRejectsBadSessionID(t *testing.T) {
	svc := netService(t.TempDir(), nil)

	_, err := list(t, svc, &v1.ListNetworkEventsRequest{SessionId: "../x"})

	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "%v", err)
}

func TestListNetworkEventsNoSandboxPin(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable", "sessions", netSession), 0o700))
	svc := netService(home, nil)

	_, err := list(t, svc, &v1.ListNetworkEventsRequest{SessionId: netSession})

	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err), "%v", err)
	require.Contains(t, err.Error(), "no sandbox")
}

func TestListNetworkEventsFromGatewayLog(t *testing.T) {
	home := t.TempDir()
	writePin(t, home)
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	svc := netService(home, func(_ context.Context, name string, _ time.Time) ([]openshell.Decision, error) {
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
	require.False(t, c.StoreAvailable)
	require.Empty(t, res.NextCursor)
	require.NotNil(t, c.GatewayLogStart, "the log starts after the 24h window start")
}

func TestListNetworkEventsGatewayErrorHidesDetail(t *testing.T) {
	home := t.TempDir()
	writePin(t, home)
	svc := netService(home, func(context.Context, string, time.Time) ([]openshell.Decision, error) {
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
	require.False(t, c.Resumable)
}

func TestListNetworkEventsWithoutGatewayReader(t *testing.T) {
	home := t.TempDir()
	writePin(t, home)

	res, err := list(t, netService(home, nil), &v1.ListNetworkEventsRequest{SessionId: netSession})

	require.NoError(t, err)
	require.NotEmpty(t, res.Coverage.GatewayError)
}

func TestListNetworkEventsValidatesRequest(t *testing.T) {
	home := t.TempDir()
	writePin(t, home)
	svc := netService(home, nil)

	tests := []struct {
		name string
		req  *v1.ListNetworkEventsRequest
	}{
		{"negative limit", &v1.ListNetworkEventsRequest{Limit: -1}},
		{"limit over max", &v1.ListNetworkEventsRequest{Limit: 1001}},
		{"zero since_ago", &v1.ListNetworkEventsRequest{Since: &v1.ListNetworkEventsRequest_SinceAgo{SinceAgo: durationpb.New(0)}}},
		{"negative since_ago", &v1.ListNetworkEventsRequest{Since: &v1.ListNetworkEventsRequest_SinceAgo{SinceAgo: durationpb.New(-time.Hour)}}},
		{"cursor", &v1.ListNetworkEventsRequest{After: "!!!"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.SessionId = netSession
			_, err := list(t, svc, tc.req)
			require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "%v", err)
		})
	}
}

func TestListNetworkEventsLimitTruncates(t *testing.T) {
	home := t.TempDir()
	writePin(t, home)
	start := time.Now().Add(-time.Hour)
	var ds []openshell.Decision
	for i := range 5 {
		ds = append(ds, openshell.Decision{Time: start.Add(time.Duration(i) * time.Second), Allowed: true, Kind: "HTTP", Host: fmt.Sprintf("h%d.com", i)})
	}
	svc := netService(home, func(context.Context, string, time.Time) ([]openshell.Decision, error) { return ds, nil })

	res, err := list(t, svc, &v1.ListNetworkEventsRequest{SessionId: netSession, Limit: 2})

	require.NoError(t, err)
	require.Len(t, res.Events, 2)
	require.Equal(t, "h4.com", res.Events[1].Host)
	require.True(t, res.Truncated)
	require.Empty(t, res.NextCursor)
}

func TestListNetworkEventsSinceTimeSetsWindow(t *testing.T) {
	home := t.TempDir()
	writePin(t, home)
	since := time.Now().Add(-time.Hour).Truncate(time.Second)
	var got time.Time
	svc := netService(home, func(_ context.Context, _ string, s time.Time) ([]openshell.Decision, error) {
		got = s
		return nil, nil
	})

	res, err := list(t, svc, &v1.ListNetworkEventsRequest{
		SessionId: netSession,
		Since:     &v1.ListNetworkEventsRequest_SinceTime{SinceTime: timestamppb.New(since)},
	})

	require.NoError(t, err)
	require.True(t, since.Equal(got))
	require.True(t, since.Equal(res.Coverage.Since.AsTime()))
}
