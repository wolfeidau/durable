// Package pin writes immutable session pin files atomically, with first-writer-wins semantics.
package pin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteOnce durably creates path with content unless it already exists, in which case won is false.
func WriteOnce(path string, content []byte) (won bool, err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "pin-*.tmp")
	if err != nil {
		return false, fmt.Errorf("failed to create pin file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("failed to set pin file mode: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("failed to write pin file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("failed to sync pin file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("failed to close pin file: %w", err)
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("failed to pin %s: %w", filepath.Base(path), err)
	}
	return true, nil
}

// ReadJSON reads a JSON pin file; ok is false when path does not exist.
func ReadJSON[T any](path, what string) (v T, ok bool, err error) {
	b, err := os.ReadFile(path) //nolint:gosec // path is a fixed pin file inside a session directory
	if errors.Is(err, fs.ErrNotExist) {
		return v, false, nil
	}
	if err != nil {
		return v, false, fmt.Errorf("failed to read pinned %s: %w", what, err)
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return v, false, fmt.Errorf("failed to parse pinned %s %s: %w", what, path, err)
	}
	return v, true, nil
}

// EnsureJSON pins a value as JSON on first start; existed pins empty for a session that predates the pin.
// pinned reports that the value is not from this run's discovery: an earlier pin, a lost race, or an older session.
func EnsureJSON[T any](path, what string, existed bool, empty T, discover func() (T, error)) (v T, pinned bool, err error) {
	if v, ok, err := ReadJSON[T](path, what); ok || err != nil {
		return v, ok, err
	}

	v = empty
	if !existed {
		if v, err = discover(); err != nil {
			return empty, false, err
		}
	}
	data, err := json.Marshal(v)
	if err != nil {
		return empty, false, fmt.Errorf("failed to encode %s: %w", what, err)
	}
	won, err := WriteOnce(path, data)
	if err != nil {
		return empty, false, err
	}
	if !won {
		// Another process pinned first; its value wins so both agree.
		v, ok, err := ReadJSON[T](path, what)
		if err != nil {
			return empty, false, err
		}
		if !ok {
			return empty, false, fmt.Errorf("pinned %s disappeared", what)
		}
		return v, true, nil
	}
	return v, existed, nil
}
