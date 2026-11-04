package durable

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrSessionLive means Restore found a live lease on the destination session.
var ErrSessionLive = errors.New("durable: destination session is live")

// Epoch identifies this copy of the session. A restore rotates it, so a client
// cursor from before the restore can tell that seqs were reused.
func (h *Harness) Epoch() string { return h.epoch }

// Snapshot writes a consistent copy of the session to dst, which must not exist.
// It is a read, so it works while a run holds the lease.
func (h *Harness) Snapshot(ctx context.Context, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("durable: snapshot destination %s already exists", dst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to stat snapshot destination: %w", err)
	}
	if _, err := h.db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("failed to snapshot session: %w", err)
	}
	return nil
}

// Restore replaces the session at dst with the snapshot at src under a new
// epoch. It returns ErrSessionLive while dst holds a live lease.
func Restore(ctx context.Context, src, dst string) error {
	live, err := leaseLive(ctx, dst)
	if err != nil {
		return err
	}
	if live {
		return ErrSessionLive
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".restore-*")
	if err != nil {
		return fmt.Errorf("failed to create restore file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := copyInto(tmp, src); err != nil {
		return err
	}

	if err := rotateEpoch(ctx, tmpPath); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(dst + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("failed to remove stale %s: %w", suffix, err)
		}
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return fmt.Errorf("failed to move restored session into place: %w", err)
	}
	// The rename must survive power loss.
	dir, err := os.Open(filepath.Dir(dst))
	if err != nil {
		return fmt.Errorf("failed to open session directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("failed to sync session directory: %w", err)
	}
	return nil
}

func copyInto(out *os.File, src string) error {
	in, err := os.Open(src) //nolint:gosec // the caller names the snapshot
	if err != nil {
		_ = out.Close()
		return fmt.Errorf("failed to open snapshot: %w", err)
	}
	defer in.Close()
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("failed to copy snapshot: %w", err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("failed to sync restored session: %w", err)
	}
	return out.Close()
}

func leaseLive(ctx context.Context, path string) (bool, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("failed to stat destination session: %w", err)
	}
	db, err := openSQL(path)
	if err != nil {
		return false, fmt.Errorf("failed to open destination session: %w", err)
	}
	defer db.Close()
	var hasLease bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'lease')`).Scan(&hasLease); err != nil {
		return false, fmt.Errorf("failed to inspect destination session: %w", err)
	}
	if !hasLease {
		return false, nil
	}
	var live bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM lease WHERE expires_at > ?)`, nowMillis()).Scan(&live); err != nil {
		return false, fmt.Errorf("failed to read destination lease: %w", err)
	}
	return live, nil
}

func rotateEpoch(ctx context.Context, path string) (err error) {
	db, err := openSQL(path)
	if err != nil {
		return fmt.Errorf("failed to open restored session: %w", err)
	}
	defer func() {
		if cerr := db.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("failed to close restored session: %w", cerr)
		}
	}()
	// An old snapshot may predate the epoch table.
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("failed to apply schema to restored session: %w", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO session_epoch (id, epoch) VALUES (1, ?) ON CONFLICT (id) DO UPDATE SET epoch = excluded.epoch`, rand.Text()); err != nil {
		return fmt.Errorf("failed to write epoch: %w", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM lease`); err != nil {
		return fmt.Errorf("failed to clear lease: %w", err)
	}
	return nil
}
