package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
)

func TestServerInfoReportsSettingsWithoutSecrets(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `{"provider": {"name": "openrouter", "model": "vendor/model", "api_key": "env:DURABLE_TEST_KEY"}}`)
	r := startEnv(t, home, []string{"DURABLE_TEST_KEY=sk-test-secret-0123456789", "DURABLE_GITHUB_TOKEN="})
	c := newClient(r)

	resp, err := c.GetServerInfo(t.Context(), authed(c, &v1.GetServerInfoRequest{}))

	require.NoError(t, err)
	got := map[string]*v1.Setting{}
	for _, s := range resp.Msg.GetSettings() {
		got[s.GetField()] = s
	}
	require.Equal(t, "vendor/model", got["provider.model"].GetValue())
	require.Equal(t, "config.json", got["provider.model"].GetSource())
	require.Equal(t, "env DURABLE_TEST_KEY", got["provider.api_key"].GetValue())
	require.Equal(t, "default", got["listen.heartbeat"].GetSource())
	var grants []string
	for _, g := range resp.Msg.GetGrants() {
		grants = append(grants, g.GetName())
	}
	require.Contains(t, grants, "go-modules")
	body := resp.Msg.String()
	require.NotContains(t, body, "op://")
	require.NotContains(t, body, "sk-test-secret-0123456789")
}

func TestServerInfoWarnsAboutANonLoopbackAddress(t *testing.T) {
	r := startIn(t, t.TempDir(), "--addr", "0.0.0.0:0")
	c := newClient(r)

	resp, err := c.GetServerInfo(t.Context(), authed(c, &v1.GetServerInfoRequest{}))

	require.NoError(t, err)
	require.Len(t, resp.Msg.GetWarnings(), 1)
	require.Contains(t, resp.Msg.GetWarnings()[0], "non-loopback")
}
