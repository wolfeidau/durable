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
		replay:   durable.ReplaySafe,
		readOnly: true,
		run:      func(ctx context.Context, call durable.ToolCall) (string, error) { return runGrep(ctx, root, call) },
	}
}

// grepState accumulates matches across the files of one search.
type grepState struct {
	re    *regexp.Regexp
	sb    strings.Builder
	count int
}

func runGrep(ctx context.Context, root string, call durable.ToolCall) (string, error) {
	var in struct {
		Pattern string  `json:"pattern"`
		Path    *string `json:"path"`
		Glob    *string `json:"glob"`
	}
	if err := decode(call.Input, &in); err != nil {
		return "", err
	}
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid pattern: %w", err)
	}
	glob := orDefault(in.Glob, "*")
	if _, err := filepath.Match(glob, ""); err != nil {
		return "", fmt.Errorf("invalid glob: %w", err)
	}
	p, err := resolve(root, orDefault(in.Path, "."))
	if err != nil {
		return "", err
	}

	st := &grepState{re: re}
	err = walk(ctx, p, "failed to search files", func(path string, d fs.DirEntry) error {
		if !d.Type().IsRegular() {
			return nil
		}
		if ok, _ := filepath.Match(glob, d.Name()); !ok {
			return nil
		}
		err := st.file(ctx, path, relPath(root, path))
		// An unreadable file below the search path is skipped; the path itself must be readable.
		if err != nil && path != p && !errors.Is(err, errCapped) && ctx.Err() == nil {
			return nil
		}
		return err
	})
	if err != nil {
		return "", err
	}
	if st.count == 0 {
		return "no matches", nil
	}
	return st.sb.String(), nil
}

func (st *grepState) file(ctx context.Context, path, rel string) error {
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
		if n%ctxCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		line := sc.Text()
		if !st.re.MatchString(line) {
			continue
		}
		if st.count == maxGrepMatches {
			fmt.Fprintf(&st.sb, "[truncated: more than %d matches; narrow the pattern, path or glob]\n", maxGrepMatches)
			return errCapped
		}
		st.count++
		if len(line) > maxGrepLine {
			line = line[:maxGrepLine] + "..."
		}
		fmt.Fprintf(&st.sb, "%s:%d:%s\n", rel, n, line)
	}
	// A line over the scanner limit just ends the scan of this file.
	if err := sc.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return err
	}
	return nil
}
