package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChatRefusesSandboxedSession(t *testing.T) {
	ws, home := newWorkspace(t)
	marker := filepath.Join(ws, "marker.txt")
	api := newFakeAPI(t, "echo ran >> marker.txt")

	a := startAgent(t, binPath, ws, home, api.srv.URL, "--yes")
	runPrompt(t, api, 2, a, "first")
	require.Len(t, readLinesOf(t, marker), 1)

	require.NoError(t, os.WriteFile(filepath.Join(sessionDir(t, ws), "sandbox.json"), []byte(`{"name":"ds-0123456789abcdef","image":"img","token":"t"}`), 0o600))
	b := startAgent(t, binPath, ws, home, api.srv.URL, "--yes")
	require.NoError(t, b.stdin.Close())

	require.Error(t, waitExit(t, b), b.out.String())
	require.Contains(t, b.out.String(), "this session runs in a sandbox")
	require.Len(t, api.requests(), 2, "no model call, no tool run on the host")
	require.Len(t, readLinesOf(t, marker), 1)
}
