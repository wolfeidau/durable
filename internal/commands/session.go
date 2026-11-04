package commands

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const sessionMetaFile = "session.json"

// SessionMeta names a session, because its directory is a one-way hash.
type SessionMeta struct {
	CWD       string    `json:"cwd"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// ID is the session directory name, the id the API uses.
	ID string `json:"-"`
}

func createSessionDir(dir string, m SessionMeta) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}
	return WriteSessionMeta(dir, m)
}

// WriteSessionMeta writes session.json atomically, and leaves an existing one alone to keep its CreatedAt.
func WriteSessionMeta(dir string, m SessionMeta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("failed to encode session metadata: %w", err)
	}
	_, err = writePin(dir, filepath.Join(dir, sessionMetaFile), data)
	return err
}

// ListSessions reads every session.json under home, skipping directories without one or without a database.
// A malformed session.json is skipped with a warning.
func ListSessions(home string) (sessions []SessionMeta, warnings []string, err error) {
	files, err := filepath.Glob(filepath.Join(home, ".durable", "sessions", "*", sessionMetaFile))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list sessions: %w", err)
	}
	for _, f := range files {
		dir := filepath.Dir(f)
		// A first start that failed leaves session.json but no database.
		if _, err := os.Stat(filepath.Join(dir, "session.sqlite")); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, nil, fmt.Errorf("failed to check session database: %w", err)
		}
		b, err := os.ReadFile(f) //nolint:gosec // fixed name inside the session directory
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read session metadata: %w", err)
		}
		var m SessionMeta
		if err := json.Unmarshal(b, &m); err != nil {
			warnings = append(warnings, fmt.Sprintf("skipping malformed %s: %v", f, err))
			continue
		}
		m.ID = filepath.Base(dir)
		sessions = append(sessions, m)
	}
	return sessions, warnings, nil
}

// SessionPath returns the database path for a working directory and session name.
func SessionPath(cwd, session string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to find home directory: %w", err)
	}
	sum := sha256.Sum256([]byte(cwd + "\x00" + session))
	return filepath.Join(home, ".durable", "sessions", hex.EncodeToString(sum[:])[:16], "session.sqlite"), nil
}
