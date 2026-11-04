package main

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
)

func TestShutdownEndsOpenStreamsWithUnavailable(t *testing.T) {
	_, home, id, _ := seedSession(t, textReply("hi"), 0, 1)
	r := startIn(t, home)

	w := newWatcher(r, id)
	ctx, cancel := context.WithTimeout(t.Context(), watchTimeout)
	defer cancel()
	stream, err := w.client.WatchEvents(ctx, w.request("", 0))
	require.NoError(t, err)
	defer stream.Close()
	for w.done() < 1 {
		require.True(t, stream.Receive(), "%v", stream.Err())
		if e := stream.Msg().GetEvent(); e != nil {
			w.events = append(w.events, e)
		}
	}

	start := time.Now()
	require.NoError(t, r.cmd.Process.Signal(syscall.SIGTERM))
	require.False(t, stream.Receive())
	require.Equal(t, connect.CodeUnavailable, connect.CodeOf(stream.Err()), "%v", stream.Err())
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not exit while a stream was open")
	}
	require.Less(t, time.Since(start), 5*time.Second)
	require.True(t, r.cmd.ProcessState.Success(), "exit: %v", r.cmd.ProcessState)
	_, err = os.Stat(filepath.Join(home, ".durable", "server.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
