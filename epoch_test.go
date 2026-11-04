package durable

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func newEpochSession(t *testing.T, path string) *Harness {
	t.Helper()
	h, err := Open(context.Background(), path, Options{Model: &scriptedModel{}, LeaseTTL: ttl})
	require.NoError(t, err)
	return h
}

func TestEpochStableAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	h := newEpochSession(t, path)
	epoch := h.Epoch()
	require.NotEmpty(t, epoch)
	require.NoError(t, h.Close())

	h = newEpochSession(t, path)
	defer h.Close()
	require.Equal(t, epoch, h.Epoch())
}

func TestOldSchemaGainsEpoch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = db.Exec(preEventsSchema)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	h := newEpochSession(t, path)
	defer h.Close()
	require.NotEmpty(t, h.Epoch())
}

func TestSnapshotMatchesOpenSession(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	h := newEpochSession(t, filepath.Join(dir, "a.sqlite"))
	defer h.Close()
	_, err := h.Submit(ctx, "req-1", "hello")
	require.NoError(t, err)

	snap := filepath.Join(dir, "snap.sqlite")
	require.NoError(t, h.Snapshot(ctx, snap))
	require.Error(t, h.Snapshot(ctx, snap))

	want, err := h.Events(ctx, 0, 0)
	require.NoError(t, err)
	require.NotEmpty(t, want)

	copied := newEpochSession(t, snap)
	defer copied.Close()
	got, err := copied.Events(ctx, 0, 0)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestRestoreRotatesEpoch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.sqlite")
	dstPath := filepath.Join(dir, "dst.sqlite")

	src := newEpochSession(t, srcPath)
	_, err := src.Submit(ctx, "req-1", "hello")
	require.NoError(t, err)
	snap := filepath.Join(dir, "snap.sqlite")
	require.NoError(t, src.Snapshot(ctx, snap))
	srcEpoch := src.Epoch()
	require.NoError(t, src.Close())

	old := newEpochSession(t, dstPath)
	oldEpoch := old.Epoch()
	require.NoError(t, old.Close())

	require.NoError(t, Restore(ctx, snap, dstPath))
	h := newEpochSession(t, dstPath)
	first := h.Epoch()
	require.NotEqual(t, srcEpoch, first)
	require.NotEqual(t, oldEpoch, first)
	evs, err := h.Events(ctx, 0, 0)
	require.NoError(t, err)
	require.NotEmpty(t, evs)
	_, err = h.Submit(ctx, "req-2", "again")
	require.NoError(t, err)
	require.NoError(t, h.Close())

	require.NoError(t, Restore(ctx, snap, dstPath))
	h = newEpochSession(t, dstPath)
	defer h.Close()
	require.NotEqual(t, first, h.Epoch())
}

func TestRestoreRefusesLiveSession(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := newEpochSession(t, filepath.Join(dir, "src.sqlite"))
	snap := filepath.Join(dir, "snap.sqlite")
	require.NoError(t, src.Snapshot(ctx, snap))
	require.NoError(t, src.Close())

	dstPath := filepath.Join(dir, "dst.sqlite")
	live := newEpochSession(t, dstPath)
	defer live.Close()
	epoch := live.Epoch()

	require.ErrorIs(t, Restore(ctx, snap, dstPath), ErrSessionLive)
	require.Equal(t, epoch, live.Epoch())
	var stored string
	require.NoError(t, live.db.QueryRowContext(ctx, `SELECT epoch FROM session_epoch`).Scan(&stored))
	require.Equal(t, epoch, stored)
}
