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

	got, _, err := ListSessions(home)
	require.NoError(t, err)
	require.Empty(t, got, "missing .durable")

	sessions := filepath.Join(home, ".durable", "sessions")
	require.NoError(t, os.MkdirAll(sessions, 0o700))
	got, _, err = ListSessions(home)
	require.NoError(t, err)
	require.Empty(t, got, "empty sessions directory")

	require.NoError(t, os.MkdirAll(filepath.Join(sessions, "nometa"), 0o700))
	withMeta := filepath.Join(sessions, "withmeta")
	require.NoError(t, os.MkdirAll(withMeta, 0o700))
	want := SessionMeta{CWD: "/a", Name: "one", CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	require.NoError(t, WriteSessionMeta(withMeta, want))

	got, _, err = ListSessions(home)
	require.NoError(t, err)
	require.Empty(t, got, "no database yet, as after a failed first start")

	require.NoError(t, os.WriteFile(filepath.Join(withMeta, "session.sqlite"), nil, 0o600))
	got, _, err = ListSessions(home)
	require.NoError(t, err)
	want.ID = "withmeta"
	require.Equal(t, []SessionMeta{want}, got)
}

func TestListSessionsSkipsMalformedFileWithWarning(t *testing.T) {
	home := t.TempDir()
	sessions := filepath.Join(home, ".durable", "sessions")
	bad := filepath.Join(sessions, "bad")
	good := filepath.Join(sessions, "good")
	for _, d := range []string{bad, good} {
		require.NoError(t, os.MkdirAll(d, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(d, "session.sqlite"), nil, 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(bad, sessionMetaFile), []byte("{nope"), 0o600))
	want := SessionMeta{CWD: "/a", Name: "one", CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	require.NoError(t, WriteSessionMeta(good, want))

	got, warnings, err := ListSessions(home)
	require.NoError(t, err)
	want.ID = "good"
	require.Equal(t, []SessionMeta{want}, got)
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], filepath.Join(bad, sessionMetaFile))
}
