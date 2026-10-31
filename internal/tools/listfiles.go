package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
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
		replay: durable.ReplaySafe,
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			var in struct {
				Path *string `json:"path"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			dir := "."
			if in.Path != nil {
				dir = *in.Path
			}
			p, err := resolve(root, dir)
			if err != nil {
				return "", err
			}

			var sb strings.Builder
			count := 0
			errCapped := errors.New("capped")
			err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if cerr := ctx.Err(); cerr != nil {
					return cerr
				}
				if path == p {
					return nil
				}
				if d.IsDir() && d.Name() == ".git" {
					return filepath.SkipDir
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
			if err != nil && !errors.Is(err, errCapped) {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				return "", fmt.Errorf("failed to list files: %w", err)
			}
			return sb.String(), nil
		},
	}
}
