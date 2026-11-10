package sandbox_test

import (
	"bytes"
	"io"
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
