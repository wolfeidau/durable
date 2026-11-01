package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
)

func TestBashOutput(t *testing.T) {
	e := newEnv(t)
	out, err := e.run(t, "bash", map[string]any{"command": "echo out; echo err >&2; pwd", "timeout_seconds": nil})
	require.NoError(t, err)
	require.Contains(t, out, "out\n")
	require.Contains(t, out, "err\n")
	require.Contains(t, out, e.root)
}

func TestBashNonZeroExit(t *testing.T) {
	e := newEnv(t)
	_, err := e.run(t, "bash", map[string]any{"command": "echo boom; exit 3", "timeout_seconds": nil})
	require.ErrorContains(t, err, "code 3")
	require.ErrorContains(t, err, "boom")
}

func TestBashStdinIsEmpty(t *testing.T) {
	e := newEnv(t)
	out, err := e.run(t, "bash", map[string]any{"command": "cat; echo done", "timeout_seconds": nil})
	require.NoError(t, err)
	require.Equal(t, "done\n", out)
}

func TestBashScrubsCredentials(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "leak-me")
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "leak-me")
	t.Setenv("OPENROUTER_API_KEY", "leak-me")
	t.Setenv("DURABLE_KEEP", "kept")
	e := newEnv(t)
	out, err := e.run(t, "bash", map[string]any{"command": "env", "timeout_seconds": nil})
	require.NoError(t, err)
	require.NotContains(t, out, "leak-me")
	require.Contains(t, out, "DURABLE_KEEP=kept")
}

func TestBashTimeoutBounds(t *testing.T) {
	e := newEnv(t)
	_, err := e.run(t, "bash", map[string]any{"command": "true", "timeout_seconds": 601})
	require.ErrorContains(t, err, "timeout_seconds")
	_, err = e.run(t, "bash", map[string]any{"command": "true", "timeout_seconds": 0})
	require.ErrorContains(t, err, "timeout_seconds")
}

func TestBashTimeout(t *testing.T) {
	e := newEnv(t)
	start := time.Now()
	out, err := e.run(t, "bash", map[string]any{"command": "echo started; sleep 30", "timeout_seconds": 1})
	require.ErrorContains(t, err, "timed out")
	require.ErrorContains(t, err, "started")
	require.Empty(t, out)
	require.NotErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 10*time.Second)
}

func processGone(pid int) bool {
	return syscall.Kill(pid, 0) == syscall.ESRCH
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		b, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			return false
		}
		pid = n
		return true
	}, 5*time.Second, 20*time.Millisecond)
	return pid
}

func TestBashParentCancelKillsChildren(t *testing.T) {
	e := newEnv(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := "(echo $BASHPID > " + pidFile + "; exec sleep 60) & wait"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	input, err := json.Marshal(map[string]any{"command": cmd, "timeout_seconds": nil})
	require.NoError(t, err)
	bash := e.tools["bash"]
	go func() {
		_, err := bash.Execute(ctx, durable.ToolCall{ID: "1", Name: "bash", Input: input})
		errCh <- err
	}()

	pid := readPid(t, pidFile)
	require.False(t, processGone(pid))
	cancel()

	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(10 * time.Second):
		t.Fatal("bash did not return after cancel")
	}
	require.Eventually(t, func() bool { return processGone(pid) }, 5*time.Second, 20*time.Millisecond)
}

func TestBashTimeoutKillsChildren(t *testing.T) {
	e := newEnv(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := "(echo $BASHPID > " + pidFile + "; exec sleep 60) & wait"
	_, err := e.run(t, "bash", map[string]any{"command": cmd, "timeout_seconds": 1})
	require.ErrorContains(t, err, "timed out")
	pid := readPid(t, pidFile)
	require.Eventually(t, func() bool { return processGone(pid) }, 5*time.Second, 20*time.Millisecond)
}

func TestBashOutputCap(t *testing.T) {
	e := newEnv(t)
	cmd := `printf 'HEAD'; head -c 200000 /dev/zero | tr '\0' x; printf 'TAIL'`
	out, err := e.run(t, "bash", map[string]any{"command": cmd, "timeout_seconds": nil})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(out, "HEAD"))
	require.True(t, strings.HasSuffix(out, "TAIL"))
	require.Contains(t, out, "bytes omitted")
	require.Less(t, len(out), maxOutputBytes+100)
}

func TestBoundedWriterExact(t *testing.T) {
	w := newBoundedWriter(10)
	_, err := w.Write([]byte("0123456789"))
	require.NoError(t, err)
	require.Equal(t, "0123456789", w.String())
	for _, s := range []string{"abc", "defghijklmnop", "XYZ"} {
		_, err = w.Write([]byte(s))
		require.NoError(t, err)
	}
	require.Equal(t, "01234\n... [19 bytes omitted] ...\nopXYZ", w.String())
}

func TestBashChildHoldingStdoutDoesNotHang(t *testing.T) {
	e := newEnv(t)
	start := time.Now()
	out, err := e.run(t, "bash", map[string]any{"command": "sleep 20 & echo hi", "timeout_seconds": nil})
	require.NoError(t, err)
	require.Equal(t, "hi\n", out)
	require.Less(t, time.Since(start), 10*time.Second)
}

func TestTruncate(t *testing.T) {
	require.Equal(t, "short", Truncate("short", 10))

	got := Truncate(strings.Repeat("a", 10)+strings.Repeat("b", 10), 10)
	require.Equal(t, "aaaaa\n... [10 bytes omitted] ...\nbbbbb", got)
}
