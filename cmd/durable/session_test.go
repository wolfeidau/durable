package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/commands"
)

func TestSessionIndexIsWrittenOnce(t *testing.T) {
	ws, home := newWorkspace(t)
	chatOnce(t, binPath, ws, home, textReply("hi"), "go", 1, "--session", "indexed")

	got, warnings, err := commands.ListSessions(home)
	require.NoError(t, err)
	require.Empty(t, warnings)
	require.Len(t, got, 1)
	require.Equal(t, "indexed", got[0].Name)
	require.Equal(t, ws, got[0].CWD)
	require.False(t, got[0].CreatedAt.IsZero())

	chatOnce(t, binPath, ws, home, textReply("again"), "go again", 1, "--session", "indexed")
	again, _, err := commands.ListSessions(home)
	require.NoError(t, err)
	require.Equal(t, got, again)
}

func TestSessionIndexIsBackfilled(t *testing.T) {
	ws, home := newWorkspace(t)
	chatOnce(t, binPath, ws, home, textReply("hi"), "go", 1, "--session", "old")

	dbPath, err := commands.SessionPath(ws, "old")
	require.NoError(t, err)
	dir := filepath.Dir(dbPath)
	require.NoError(t, os.Remove(filepath.Join(dir, "session.json")))

	chatOnce(t, binPath, ws, home, textReply("again"), "go again", 1, "--session", "old")

	got, warnings, err := commands.ListSessions(home)
	require.NoError(t, err)
	require.Empty(t, warnings)
	require.Len(t, got, 1)
	require.Equal(t, "old", got[0].Name)
	require.Equal(t, ws, got[0].CWD)
	require.Equal(t, filepath.Base(dir), got[0].ID)
}
