package commands

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/sandbox/sandboxtest"
)

func TestAssemblePinsGitAuthorAndReopenKeepsIt(t *testing.T) {
	assembleHome(t)
	f := sandboxtest.New(t)
	cfg := sandboxCfg(t, f, "")
	cfg.Sandbox.Environments = goEnvs()
	cfg.Sandbox.GitAuthor = sandbox.GitIdentity{Name: "durable", Email: "durable@localhost"}
	a, err := Assemble(t.Context(), cfg)
	require.NoError(t, err)
	a.Close()
	markExisting(t, cfg)

	cfg.Sandbox.GitAuthor = sandbox.GitIdentity{Name: "other", Email: "other@example.com"}
	a, err = Assemble(t.Context(), cfg)
	require.NoError(t, err)
	defer a.Close()

	want := map[string]string{"user.name": "durable", "user.email": "durable@localhost"}
	specs := f.Specs()
	require.Equal(t, want, specs[0].GitConfig)
	require.Equal(t, want, specs[1].GitConfig)
}
