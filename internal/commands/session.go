package commands

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// SessionPath returns the database path for a working directory and session name.
func SessionPath(cwd, session string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to find home directory: %w", err)
	}
	sum := sha256.Sum256([]byte(cwd + "\x00" + session))
	return filepath.Join(home, ".durable", "sessions", hex.EncodeToString(sum[:])[:16], "session.sqlite"), nil
}
