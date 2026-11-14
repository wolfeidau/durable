package openshell

import (
	"context"
	_ "embed"
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/commands"
	"github.com/wolfeidau/durable/internal/sandbox"
)

//go:embed testdata/mise/mise.toml
var miseFixtureConfig []byte

//go:embed testdata/mise/mise.lock
var miseFixtureLock []byte

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestGatewayMiseGrantInstallsFromTheLockfileOnly(t *testing.T) {
	l := gatewayLauncher(t)
	name, token := "dt-"+randHex(t, 6), randHex(t, 16)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		require.NoError(t, l.Delete(ctx, name))
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	require.NoError(t, l.Create(ctx, sandbox.Spec{Name: name, Image: testImage(), Token: token, Grants: commands.BuildGrants([]string{"mise"}, "", nil)}))
	bash := bashTool(t, dialWorker(t, l, name, token))
	n := 0
	run := func(command string) string {
		n++
		return runBash(t.Context(), t, bash, "m"+string(rune('a'+n)), command+" 2>&1; echo rc=$?")
	}

	out := run("echo " + b64(miseFixtureConfig) + " | base64 -d > mise.toml && echo " + b64(miseFixtureLock) + " | base64 -d > mise.lock")
	require.Contains(t, out, "rc=0", out)

	out = run("mise install")
	require.Contains(t, out, "rc=0", out)
	require.NotContains(t, out, "policy_denied", out)

	out = run("mise exec -- jq --version")
	require.Contains(t, out, "jq-1.7.1", out)

	// The grant is bound to the mise binary, so curl to the same release URL is refused.
	out = run("curl -sS -m 20 -o /dev/null https://github.com/jqlang/jq/releases/download/jq-1.7.1/jq-linux-arm64")
	require.NotContains(t, out, "rc=0", out)

	// mise looks versions up through mise-versions and the GitHub API, and the grant allows neither.
	out = run("mise ls-remote jq")
	require.NotContains(t, out, "rc=0", out)
	out = run("mise exec -- curl -sS -m 20 https://api.github.com/zen")
	require.NotContains(t, out, "rc=0", out)
}
