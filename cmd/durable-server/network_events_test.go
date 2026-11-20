package main

import (
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
)

func TestListNetworkEventsUnknownSession(t *testing.T) {
	t.Parallel()
	r := startIn(t, t.TempDir())
	c := newClient(r)

	_, err := c.ListNetworkEvents(t.Context(), authed(c, &v1.ListNetworkEventsRequest{SessionId: "5555555555555555"}))

	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
}

func TestListNetworkEventsReportsGatewayLog(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	home := t.TempDir()
	r := startIn(t, home)
	c := newClient(r)
	const session = "7777777777777777"
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable", "sessions", session), 0o700))

	_, err := c.ListNetworkEvents(t.Context(), authed(c, &v1.ListNetworkEventsRequest{SessionId: session}))

	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err), "%v", err)
}
