package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/wolfeidau/durable"
)

const maxListEntries = 2000

func newListFiles(root string) durable.Tool {
	return tool{
		spec: durable.ToolSpec{
			Name: "list_files",
			Description: "Recursively list files and directories under a directory, sorted, as paths relative to the workspace root (directories end in /). " +
				"Skips .git. Output is capped at 2000 entries and ends with a marker if more exist; list a subdirectory to see the rest.",
			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":["string","null"],"description":"Directory to list, or null for the workspace root."}},` +
				`"required":["path"],"additionalProperties":false}`),
		},
		replay:   durable.ReplaySafe,
		readOnly: true,
		run:      func(ctx context.Context, call durable.ToolCall) (string, error) { return runListFiles(ctx, root, call) },
	}
}

func runListFiles(ctx context.Context, root string, call durable.ToolCall) (string, error) {
	var in struct {
		Path *string `json:"path"`
	}
	if err := decode(call.Input, &in); err != nil {
		return "", err
	}
	p, err := resolve(root, orDefault(in.Path, "."))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("failed to stat path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", relPath(root, p))
	}

	var sb strings.Builder
	count := 0
	err = walk(ctx, p, "failed to list files", func(path string, d fs.DirEntry) error {
		if path == p {
			return nil
		}
		if count == maxListEntries {
			fmt.Fprintf(&sb, "[truncated: more than %d entries; list a subdirectory to see the rest]\n", maxListEntries)
			return errCapped
		}
		count++
		sb.WriteString(relPath(root, path))
		if d.IsDir() {
			sb.WriteByte('/')
		}
		sb.WriteByte('\n')
		return nil
	})
	if err != nil {
		return "", err
	}
	return sb.String(), nil
}
