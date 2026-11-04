package commands

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// ListSessions reads every session.json under home, skipping directories without one.
func ListSessions(home string) ([]SessionMeta, error) {
	files, err := filepath.Glob(filepath.Join(home, ".durable", "sessions", "*", sessionMetaFile))
	if err != nil {
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}
	var out []SessionMeta
	for _, f := range files {
		m, _, err := readJSONPin[SessionMeta](f, "session metadata")
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
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
