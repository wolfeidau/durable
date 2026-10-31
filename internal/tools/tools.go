// Package tools provides the coding agent tools, all scoped to a workspace root.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/wolfeidau/durable"
)

const binarySniffBytes = 8192

type tool struct {
	spec   durable.ToolSpec
	replay durable.Replay
	run    func(ctx context.Context, input json.RawMessage) (string, error)
}

func (t tool) Spec() durable.ToolSpec { return t.spec }

func (t tool) Replay() durable.Replay { return t.replay }

func (t tool) Execute(ctx context.Context, call durable.ToolCall) (string, error) {
	return t.run(ctx, call.Input)
}

// All returns the tools in a fixed order so the tool list is stable for prompt caching.
func All(root string) ([]durable.Tool, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve root: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve root symlinks: %w", err)
	}
	return []durable.Tool{
		newReadFile(real),
		newListFiles(real),
		newGrep(real),
		newEditFile(real),
		newBash(real),
	}, nil
}

func decode(input json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid tool input: %w", err)
	}
	return nil
}

// resolve maps a model-supplied path to a real path inside root, following symlinks.
func resolve(root, p string) (string, error) {
	if p == "" {
		return "", errors.New("path must not be empty")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	p = filepath.Clean(p)

	// Resolve the deepest existing ancestor so not-yet-existing files are checked too.
	existing, rest := p, ""
	for {
		real, err := filepath.EvalSymlinks(existing)
		if err == nil {
			p = filepath.Join(real, rest)
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("failed to resolve path %q: %w", p, err)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("failed to resolve path %q: %w", p, err)
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}

	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the workspace root", p)
	}
	return p, nil
}

func relPath(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(rel)
}

func isBinary(r io.Reader) (bool, error) {
	buf := make([]byte, binarySniffBytes)
	n, err := io.ReadFull(r, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	return bytes.IndexByte(buf[:n], 0) >= 0, nil
}
