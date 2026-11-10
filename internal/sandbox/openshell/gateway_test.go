package openshell

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/tools"
)

// These tests need a running gateway and the image from `mise run image:worker`.
const gatewayEnv = "DURABLE_TEST_OPENSHELL"

func gatewayLauncher(t *testing.T) *Launcher {
	t.Helper()
	if os.Getenv(gatewayEnv) != "1" {
		t.Skipf("set %s=1 to run against a gateway", gatewayEnv)
	}
	l, err := New(Config{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func testImage() string {
	if img := os.Getenv("DURABLE_TEST_OPENSHELL_IMAGE"); img != "" {
		return img
	}
	return "durable-worker:dev"
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

// newSandbox creates a sandbox with a random name (the gateway allows 19 lowercase characters) and deletes it at cleanup.
func newSandbox(t *testing.T, l *Launcher) (name, token string) {
	t.Helper()
	name, token = "dt-"+randHex(t, 6), randHex(t, 16)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		require.NoError(t, l.Delete(ctx, name))
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	require.NoError(t, l.Create(ctx, Spec{Name: name, Image: testImage(), Token: token}))
	return name, token
}

func dialWorker(t *testing.T, l *Launcher, name, token string) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	conn, err := l.Connect(ctx, name, token)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func bashTool(t *testing.T, c *Conn) durable.Tool {
	t.Helper()
	all, err := c.Client().Tools(t.Context())
	require.NoError(t, err)
	for _, tl := range all {
		if tl.Spec().Name == "bash" {
			return tl
		}
	}
	require.FailNow(t, "worker has no bash tool")
	return nil
}

func runBash(ctx context.Context, t *testing.T, bash durable.Tool, id, command string) string {
	t.Helper()
	input, err := json.Marshal(map[string]any{"command": command, "timeout_seconds": nil})
	require.NoError(t, err)
	out, err := bash.Execute(ctx, durable.ToolCall{ID: id, Name: "bash", Input: input})
	require.NoError(t, err)
	return out
}

func TestGatewayCreateAndConnect(t *testing.T) {
	l := gatewayLauncher(t)
	name, token := newSandbox(t, l)

	phase, err := l.Phase(t.Context(), name)
	require.NoError(t, err)
	require.Equal(t, "Ready", string(phase))

	got, err := dialWorker(t, l, name, token).Client().Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, got, len(tools.Catalog()))
}

func TestGatewayBashRunsAsAppUserAndStreams(t *testing.T) {
	l := gatewayLauncher(t)
	name, token := newSandbox(t, l)
	bash := bashTool(t, dialWorker(t, l, name, token))

	require.Equal(t, "1500\n", runBash(t.Context(), t, bash, "u1", "id -u"))

	var got []string
	ctx := durable.WithToolOutputSink(t.Context(), func(s string) { got = append(got, s) })
	out := runBash(ctx, t, bash, "s1", "for i in 1 2 3; do echo $i; sleep 0.3; done")
	require.Equal(t, "1\n2\n3\n", out)
	require.Equal(t, out, strings.Join(got, ""))
	require.Greater(t, len(got), 1, "output should arrive in several pieces")
}

func TestGatewayBashEnvHidesToken(t *testing.T) {
	l := gatewayLauncher(t)
	name, token := newSandbox(t, l)
	bash := bashTool(t, dialWorker(t, l, name, token))

	out := runBash(t.Context(), t, bash, "e1", "env")
	require.NotContains(t, out, "DURABLE_WORKER_TOKEN")
	require.NotContains(t, out, token)
}

func TestGatewayCancelSleep(t *testing.T) {
	l := gatewayLauncher(t)
	name, token := newSandbox(t, l)
	bash := bashTool(t, dialWorker(t, l, name, token))

	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(500*time.Millisecond, cancel)
	start := time.Now()
	_, err := bash.Execute(ctx, durable.ToolCall{ID: "k1", Name: "bash", Input: []byte(`{"command":"sleep 60","timeout_seconds":null}`)})
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(start), 15*time.Second)
}

func TestGatewayStopStartKeepsWorkspace(t *testing.T) {
	l := gatewayLauncher(t)
	name, token := newSandbox(t, l)
	first := dialWorker(t, l, name, token)
	runBash(t.Context(), t, bashTool(t, first), "w1", "echo kept > /sandbox/keep.txt")
	require.NoError(t, first.Close())

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	require.NoError(t, l.Stop(ctx, name))
	phase, err := l.Phase(ctx, name)
	require.NoError(t, err)
	require.Equal(t, "Stopped", string(phase))

	require.NoError(t, l.Start(ctx, name))
	second := dialWorker(t, l, name, token)
	require.Equal(t, "kept\n", runBash(t.Context(), t, bashTool(t, second), "w2", "cat /sandbox/keep.txt"))
}

func TestGatewayDelete(t *testing.T) {
	l := gatewayLauncher(t)
	name, _ := newSandbox(t, l)

	require.NoError(t, l.Delete(t.Context(), name))
	require.Eventually(t, func() bool {
		_, err := l.Phase(t.Context(), name)
		return err != nil
	}, time.Minute, time.Second)
	require.NoError(t, l.Delete(t.Context(), name), "deleting twice is not an error")
}

func TestGatewayMissingImageFailsFast(t *testing.T) {
	l := gatewayLauncher(t)
	name := "dt-" + randHex(t, 6)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		require.NoError(t, l.Delete(ctx, name))
	})

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	err := l.Create(ctx, Spec{Name: name, Image: "registry.invalid/durable/does-not-exist:0", Token: "t"})
	require.Error(t, err)
	require.NoError(t, ctx.Err(), "a missing image should fail before the deadline")
	require.False(t, IsUnavailable(err), "a missing image is configuration, not an outage: %v", err)
}
