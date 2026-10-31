package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Notes added to a bash result when its process outlived the agent.
const (
	OrphanStopped = "The command was still running when the agent restarted and has been stopped."
	OrphanRunning = "The command may still be running and could not be stopped. Check the current state before relying on it."
)

const orphanPoll = 20 * time.Millisecond

// statePath hashes the call ID because it comes from the model provider.
func statePath(dir, callID string) string {
	sum := sha256.Sum256([]byte(callID))
	return filepath.Join(dir, hex.EncodeToString(sum[:]))
}

// lockFile holds an flock that the command inherits; the kernel drops it when the last holder exits.
type lockFile struct {
	f    *os.File
	path string
}

func openLock(path string) (*lockFile, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to create command state file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("failed to lock command state file: %w", err)
	}
	return &lockFile{f: f, path: path}, nil
}

func (l *lockFile) writeGroup(pgid int) error {
	_, err := l.f.WriteAt([]byte(strconv.Itoa(pgid)), 0)
	return err
}

func (l *lockFile) release() {
	_ = os.Remove(l.path)
	_ = l.f.Close()
}

func tryLock(f *os.File) bool {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}

// recoverBash only signals the recorded group while the lock proves the command is alive, because group IDs get reused.
func recoverBash(ctx context.Context, path string) string {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return ""
	}
	defer f.Close()

	if tryLock(f) {
		_ = os.Remove(path)
		return ""
	}

	b, _ := os.ReadFile(path)
	pgid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pgid <= 1 {
		return OrphanRunning
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return OrphanRunning
	}

	deadline := time.NewTimer(waitDelay)
	defer deadline.Stop()
	tick := time.NewTicker(orphanPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-deadline.C:
			return OrphanRunning
		case <-tick.C:
			if tryLock(f) {
				_ = os.Remove(path)
				return OrphanStopped
			}
		}
	}
}
