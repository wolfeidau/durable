package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/wolfeidau/durable"
)

const (
	maxGrepMatches = 500
	maxGrepLine    = 500
)

func newGrep(root string) durable.Tool {
	return tool{
		spec: durable.ToolSpec{
			Name: "grep",
			Description: "Search file contents with a Go (RE2) regular expression. Walks the directory in sorted order, skipping .git, binary files and symlinks. " +
				"Output lines are path:line:text, with text cut to 500 characters. Capped at 500 matches with a marker if more exist.",
			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"pattern":{"type":"string","description":"Go regular expression matched against each line."},` +
				`"path":{"type":["string","null"],"description":"File or directory to search, or null for the workspace root."},` +
				`"glob":{"type":["string","null"],"description":"Only search files whose base name matches this glob (e.g. *.go), or null for all files."}},` +
				`"required":["pattern","path","glob"],"additionalProperties":false}`),
		},
		replay: durable.ReplaySafe,
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			var in struct {
				Pattern string  `json:"pattern"`
				Path    *string `json:"path"`
				Glob    *string `json:"glob"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			re, err := regexp.Compile(in.Pattern)
			if err != nil {
				return "", fmt.Errorf("invalid pattern: %w", err)
			}
			if in.Glob != nil {
				if _, err := filepath.Match(*in.Glob, ""); err != nil {
					return "", fmt.Errorf("invalid glob: %w", err)
				}
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
				if d.IsDir() {
					if d.Name() == ".git" {
						return filepath.SkipDir
					}
					return nil
				}
				if !d.Type().IsRegular() {
					return nil
				}
				if in.Glob != nil {
					if ok, _ := filepath.Match(*in.Glob, d.Name()); !ok {
						return nil
					}
				}
				return grepFile(path, relPath(root, path), re, &sb, &count, errCapped)
			})
			if err != nil && !errors.Is(err, errCapped) {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				return "", fmt.Errorf("failed to search files: %w", err)
			}
			if count == 0 {
				return "no matches", nil
			}
			return sb.String(), nil
		},
	}
}

func grepFile(path, rel string, re *regexp.Regexp, sb *strings.Builder, count *int, errCapped error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	bin, err := isBinary(f)
	if err != nil {
		return err
	}
	if bin {
		return nil
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if !re.MatchString(line) {
			continue
		}
		if *count == maxGrepMatches {
			fmt.Fprintf(sb, "[truncated: more than %d matches; narrow the pattern, path or glob]\n", maxGrepMatches)
			return errCapped
		}
		*count++
		if len(line) > maxGrepLine {
			line = line[:maxGrepLine] + "..."
		}
		fmt.Fprintf(sb, "%s:%d:%s\n", rel, n, line)
	}
	// A line over the scanner limit just ends the scan of this file.
	if err := sc.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return err
	}
	return nil
}
