package sandbox_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/sandbox/sandboxtest"
)

func openFake(t *testing.T) *sandbox.Client {
	t.Helper()
	f := sandboxtest.New(t)
	c, closer, err := f.Connect(t.Context(), "n", "tok")
	require.NoError(t, err)
	t.Cleanup(func() { _ = closer.Close() })
	return c
}

func TestClientStdioRoundTripAndClose(t *testing.T) {
	c := openFake(t)
	var stderr bytes.Buffer
	rwc, err := c.Stdio(t.Context(), sandbox.StdioSpec{Command: "sh", Args: []string{"-c", "echo err >&2; cat"}}, &stderr)
	require.NoError(t, err)

	_, err = rwc.Write([]byte("ping\n"))
	require.NoError(t, err)
	buf := make([]byte, 5)
	_, err = io.ReadFull(rwc, buf)
	require.NoError(t, err)
	require.Equal(t, "ping\n", string(buf))

	closed := make(chan struct{})
	go func() { _ = rwc.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
	_, err = rwc.Read(buf)
	require.Error(t, err)
	require.Equal(t, "err\n", stderr.String())
}

func TestClientStdioReadEndsWhenProcessExits(t *testing.T) {
	c := openFake(t)
	rwc, err := c.Stdio(t.Context(), sandbox.StdioSpec{Command: "echo", Args: []string{"bye"}}, nil)
	require.NoError(t, err)
	defer rwc.Close()
	got, err := io.ReadAll(rwc)
	require.NoError(t, err)
	require.Equal(t, "bye\n", string(got))
}

func TestClientRunReportsExitCodeAndStderr(t *testing.T) {
	c := openFake(t)

	code, stderr, err := c.Run(t.Context(), sandbox.StdioSpec{Command: "sh", Args: []string{"-c", "echo fine"}})
	require.NoError(t, err)
	require.Zero(t, code)
	require.Empty(t, stderr)

	code, stderr, err = c.Run(t.Context(), sandbox.StdioSpec{Command: "sh", Args: []string{"-c", "echo boom >&2; exit 3"}})
	require.NoError(t, err)
	require.Equal(t, 3, code)
	require.Equal(t, "boom\n", string(stderr))
}

func TestClientRunKeepsTheEndOfStderr(t *testing.T) {
	c := openFake(t)
	script := "head -c 20000 /dev/zero | tr '\\0' a >&2; echo >&2; echo last >&2"

	code, stderr, err := c.Run(t.Context(), sandbox.StdioSpec{Command: "sh", Args: []string{"-c", script}})

	require.NoError(t, err)
	require.Zero(t, code)
	require.Len(t, stderr, 8<<10)
	require.True(t, strings.HasSuffix(string(stderr), "\nlast\n"))
}

func TestClientRunCancelKillsTheProcess(t *testing.T) {
	c := openFake(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, _, err := c.Run(ctx, sandbox.StdioSpec{Command: "sh", Args: []string{"-c", "echo $$ > " + pidFile + "; exec sleep 30"}})
		done <- err
	}()
	var pid int
	require.Eventually(t, func() bool {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil
	}, 10*time.Second, 20*time.Millisecond)

	cancel()

	require.ErrorIs(t, <-done, context.Canceled)
	require.Eventually(t, func() bool { return syscall.Kill(pid, 0) != nil }, 10*time.Second, 50*time.Millisecond)
}
