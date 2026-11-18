package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
)

func wideSince() *v1.ListNetworkEventsRequest_SinceAgo {
	return &v1.ListNetworkEventsRequest_SinceAgo{SinceAgo: durationpb.New(87600 * time.Hour)}
}

func TestListNetworkEventsServedFromTelemetryStore(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, telemetryConfig)
	r := startEnv(t, home, []string{"XDG_CONFIG_HOME=" + t.TempDir()})
	addr := readOTLPAddr(t, home)
	token := readHeadersToken(t, home)
	require.Equal(t, http.StatusOK, exportLogs(t, addr, token, fixtureRecords(t)...))
	require.Equal(t, http.StatusOK, exportLogs(t, addr, token, gapRecord()))
	const session = "4444444444444444"
	writeAuditPin(t, home, session)
	c := newClient(r)

	resp, err := c.ListNetworkEvents(t.Context(), authed(c, &v1.ListNetworkEventsRequest{SessionId: session, Since: wideSince()}))

	require.NoError(t, err)
	require.Len(t, resp.Msg.Events, 4)
	var hosts []string
	for _, e := range resp.Msg.Events {
		require.NotEmpty(t, e.Id)
		hosts = append(hosts, e.Host)
	}
	require.Contains(t, hosts, "github.com")
	cov := resp.Msg.Coverage
	require.Equal(t, v1.NetworkSource_NETWORK_SOURCE_STORE, cov.Source)
	require.True(t, cov.StoreAvailable)
	require.Equal(t, int64(3), cov.DroppedOwn.Dropped)
	require.Equal(t, 720*time.Hour, cov.Retention.AsDuration())

	// Paging resumes strictly after the cursor and ends with nothing new.
	one, err := c.ListNetworkEvents(t.Context(), authed(c, &v1.ListNetworkEventsRequest{SessionId: session, Since: wideSince(), Limit: 1}))
	require.NoError(t, err)
	require.True(t, one.Msg.Truncated)
	require.Equal(t, resp.Msg.Events[3].Id, one.Msg.Events[0].Id, "the first load is the newest event")
	rest, err := c.ListNetworkEvents(t.Context(), authed(c, &v1.ListNetworkEventsRequest{SessionId: session, Since: wideSince(), After: one.Msg.NextCursor}))
	require.NoError(t, err)
	require.Empty(t, rest.Msg.Events)
	require.Equal(t, one.Msg.NextCursor, rest.Msg.NextCursor)
	require.False(t, rest.Msg.CursorReset)

	// A later insert is returned on resume even though its event time (the fixture's) is older than rows the client has.
	late := fixtureRecords(t)[1]
	late.Attributes[0] = strAttr("ocsf.metadata.uid", "late-arrival")
	require.Equal(t, http.StatusOK, exportLogs(t, addr, token, late))
	resumed, err := c.ListNetworkEvents(t.Context(), authed(c, &v1.ListNetworkEventsRequest{SessionId: session, Since: wideSince(), After: one.Msg.NextCursor}))
	require.NoError(t, err)
	require.Len(t, resumed.Msg.Events, 1)
	require.Equal(t, "late-arrival", resumed.Msg.Events[0].Id)

	_, err = c.ListNetworkEvents(t.Context(), authed(c, &v1.ListNetworkEventsRequest{SessionId: "5555555555555555"}))
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
	stopServer(t, r)
}

func TestListNetworkEventsWithoutTelemetryReportsGatewayLog(t *testing.T) {
	home := t.TempDir()
	// An empty gateway config dir makes the gateway read fail whatever the developer's machine has.
	r := startEnv(t, home, []string{"XDG_CONFIG_HOME=" + t.TempDir()})
	const session = "6666666666666666"
	writeAuditPin(t, home, session)
	c := newClient(r)

	resp, err := c.ListNetworkEvents(t.Context(), authed(c, &v1.ListNetworkEventsRequest{SessionId: session}))

	require.NoError(t, err)
	require.Empty(t, resp.Msg.Events)
	cov := resp.Msg.Coverage
	require.Equal(t, v1.NetworkSource_NETWORK_SOURCE_GATEWAY_LOG, cov.Source)
	require.False(t, cov.StoreAvailable)
	require.NotEmpty(t, cov.GatewayError)
	require.True(t, cov.SourceBounded)
	require.False(t, cov.Resumable)
	require.Empty(t, resp.Msg.NextCursor, "the gateway log issues no cursor")
}

func TestListNetworkEventsNeedsASandbox(t *testing.T) {
	home := t.TempDir()
	r := startIn(t, home)
	c := newClient(r)
	const session = "7777777777777777"
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable", "sessions", session), 0o700))

	_, err := c.ListNetworkEvents(t.Context(), authed(c, &v1.ListNetworkEventsRequest{SessionId: session}))

	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err), "%v", err)
}
