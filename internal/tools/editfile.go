package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/wolfeidau/durable"
)

func newEditFile(root string) durable.Tool {
	return tool{
		spec: durable.ToolSpec{
			Name: "edit_file",
			Description: "Edit a file by exact string replacement. `old_string` must occur exactly once in the file (include surrounding context to make it unique); " +
				"otherwise nothing is changed and an error reports the count. If `old_string` is empty the file is created with `new_string` (parent directories are created) and it is an error if the file already exists.",
			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string","description":"File path, relative to the workspace root or absolute inside it."},` +
				`"old_string":{"type":"string","description":"Exact text to replace, or an empty string to create the file."},` +
				`"new_string":{"type":"string","description":"Replacement text, or the content of the new file."}},` +
				`"required":["path","old_string","new_string"],"additionalProperties":false}`),
		},
		replay: durable.ReplayNever,
		run: func(_ context.Context, call durable.ToolCall) (string, error) {
			var in struct {
				Path      string `json:"path"`
				OldString string `json:"old_string"`
				NewString string `json:"new_string"`
			}
			if err := decode(call.Input, &in); err != nil {
				return "", err
			}
			p, err := resolve(root, in.Path)
			if err != nil {
				return "", err
			}
			if in.OldString == "" {
				return createFile(p, in.Path, in.NewString)
			}
			return replaceInFile(p, in.Path, in.OldString, in.NewString)
		},
	}
}

func createFile(p, display, content string) (string, error) {
	if _, err := os.Lstat(p); err == nil {
		return "", fmt.Errorf("%s already exists; use a non-empty old_string to edit it", display)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("failed to stat file: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", fmt.Errorf("failed to create parent directories: %w", err)
	}
	if err := writeAtomic(p, []byte(content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("created %s", display), nil
}

func replaceInFile(p, display, oldStr, newStr string) (string, error) {
	info, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("failed to stat file: %w", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}
	content := string(data)
	switch n := strings.Count(content, oldStr); n {
	case 1:
	case 0:
		return "", fmt.Errorf("old_string not found in %s (0 matches)", display)
	default:
		return "", fmt.Errorf("old_string matches %d places in %s; include more surrounding context so it is unique", n, display)
	}
	updated := strings.Replace(content, oldStr, newStr, 1)
	if err := writeAtomic(p, []byte(updated), info.Mode().Perm()); err != nil {
		return "", err
	}
	return fmt.Sprintf("edited %s", display), nil
}

func writeAtomic(path string, data []byte, mode fs.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".edit-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	if err = tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to set file mode: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}
	return nil
}
