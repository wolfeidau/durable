package server

import (
	"log/slog"
	"slices"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
)

func TestGetServerInfoReturnsSettingsWarningsAndGrants(t *testing.T) {
	e := newEnv(t)
	svc := sessionService{
		reg: e.registry(t), home: e.home, logger: slog.New(slog.DiscardHandler),
		settings: []Setting{{Field: "listen.addr", Value: "127.0.0.1:1", Source: "default"}},
		warnings: []string{"--model overrides provider.model"},
	}

	info, err := svc.GetServerInfo(t.Context(), connect.NewRequest(&v1.GetServerInfoRequest{}))

	require.NoError(t, err)
	require.Len(t, info.Msg.Settings, 1)
	require.Equal(t, "listen.addr", info.Msg.Settings[0].Field)
	require.Equal(t, "127.0.0.1:1", info.Msg.Settings[0].Value)
	require.Equal(t, "default", info.Msg.Settings[0].Source)
	require.Equal(t, []string{"--model overrides provider.model"}, info.Msg.Warnings)
	i := slices.IndexFunc(info.Msg.Grants, func(g *v1.GrantInfo) bool { return g.Name == "go-modules" })
	require.GreaterOrEqual(t, i, 0)
	g := info.Msg.Grants[i]
	require.Equal(t, "go-modules", g.Name)
	require.NotEmpty(t, g.Summary)
	require.Equal(t, []string{"/usr/local/go/bin/go"}, g.Binaries)
	require.Equal(t, []string{"proxy.golang.org:443", "sum.golang.org:443"}, g.Hosts)
}
