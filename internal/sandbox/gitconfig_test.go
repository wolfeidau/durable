package sandbox_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/sandbox"
)

func TestPinWithGitConfigRoundTripsAndOldPinsDecodeWithNone(t *testing.T) {
	dir := t.TempDir()
	conf := sandbox.GitIdentity{Name: "durable", Email: "durable@localhost"}.Config()
	require.Equal(t, map[string]string{"user.name": "durable", "user.email": "durable@localhost"}, conf)

	p, err := sandbox.WritePinGit(dir, "img", nil, nil, nil, conf)
	require.NoError(t, err)
	require.Equal(t, conf, p.Spec().GitConfig)
	again, ok, err := sandbox.ReadPin(dir)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, conf, again.GitConfig)

	old := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(old, sandbox.PinFile), []byte(`{"name":"ds-1","image":"img","token":"t"}`), 0o600))
	op, ok, err := sandbox.ReadPin(old)
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, op.GitConfig)
	require.Nil(t, sandbox.GitIdentity{}.Config())
}
