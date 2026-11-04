package commands

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWriteSessionMetaKeepsFirstWrite(t *testing.T) {
	dir := t.TempDir()
	first := SessionMeta{CWD: "/a", Name: "one", CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	require.NoError(t, WriteSessionMeta(dir, first))
	require.NoError(t, WriteSessionMeta(dir, SessionMeta{CWD: "/b", Name: "two", CreatedAt: time.Now().UTC()}))

	got, ok, err := readJSONPin[SessionMeta](filepath.Join(dir, sessionMetaFile), "session metadata")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, first, got)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "temp file must not be left behind")
}

func TestListSessions(t *testing.T) {
	home := t.TempDir()

	got, err := ListSessions(home)
	require.NoError(t, err)
	require.Empty(t, got, "missing .durable")

	sessions := filepath.Join(home, ".durable", "sessions")
	require.NoError(t, os.MkdirAll(sessions, 0o700))
	got, err = ListSessions(home)
	require.NoError(t, err)
	require.Empty(t, got, "empty sessions directory")

	require.NoError(t, os.MkdirAll(filepath.Join(sessions, "nometa"), 0o700))
	withMeta := filepath.Join(sessions, "withmeta")
	require.NoError(t, os.MkdirAll(withMeta, 0o700))
	want := SessionMeta{CWD: "/a", Name: "one", CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	require.NoError(t, WriteSessionMeta(withMeta, want))

	got, err = ListSessions(home)
	require.NoError(t, err)
	require.Equal(t, []SessionMeta{want}, got)
}

func TestListSessionsRejectsMalformedFile(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".durable", "sessions", "bad")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, sessionMetaFile), []byte("{nope"), 0o600))

	_, err := ListSessions(home)
	require.Error(t, err)
}
