package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/wolfeidau/durable"
)

const (
	defaultReadLimit = 2000
	maxLineChars     = 2000
)

func newReadFile(root string) durable.Tool {
	return tool{
		spec: durable.ToolSpec{
			Name: "read_file",
			Description: "Read a text file and return numbered lines. Returns at most `limit` lines (default 2000) starting at the 1-based line `offset` (default 1); " +
				"lines longer than 2000 characters are cut. If more lines remain the output ends with a marker giving the offset to continue from. Binary files are rejected.",
			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string","description":"File path, relative to the workspace root or absolute inside it."},` +
				`"offset":{"type":["integer","null"],"description":"1-based line to start at, or null for 1."},` +
				`"limit":{"type":["integer","null"],"description":"Maximum lines to return, or null for 2000."}},` +
				`"required":["path","offset","limit"],"additionalProperties":false}`),
		},
		replay:   durable.ReplaySafe,
		readOnly: true,
		run: func(_ context.Context, call durable.ToolCall) (string, error) {
			var in struct {
				Path   string `json:"path"`
				Offset *int   `json:"offset"`
				Limit  *int   `json:"limit"`
			}
			if err := decode(call.Input, &in); err != nil {
				return "", err
			}
			offset, limit := 1, defaultReadLimit
			if in.Offset != nil {
				offset = *in.Offset
			}
			if in.Limit != nil {
				limit = *in.Limit
			}
			if offset < 1 || limit < 1 {
				return "", errors.New("offset and limit must be at least 1")
			}
			p, err := resolve(root, in.Path)
			if err != nil {
				return "", err
			}
			return readLines(p, offset, limit)
		},
	}
}

func readLines(path string, offset, limit int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	bin, err := isBinary(f)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}
	if bin {
		return "", errors.New("file appears to be binary; refusing to read it")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("failed to rewind file: %w", err)
	}

	r := bufio.NewReader(f)
	var sb strings.Builder
	n, emitted := 0, 0
	for {
		line, err := readLine(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("failed to read file: %w", err)
		}
		n++
		if n < offset {
			continue
		}
		if emitted == limit {
			fmt.Fprintf(&sb, "[truncated: more lines remain; call read_file again with offset=%d to continue]\n", n)
			return sb.String(), nil
		}
		fmt.Fprintf(&sb, "%6d\t%s\n", n, line)
		emitted++
	}
	if emitted == 0 && offset > 1 {
		return "", fmt.Errorf("offset %d is beyond the end of the file (%d lines)", offset, n)
	}
	return sb.String(), nil
}

// readLine returns one line cut to maxLineChars, discarding the rest of an overlong line.
func readLine(r *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		chunk, more, err := r.ReadLine()
		if err != nil {
			return "", err
		}
		if room := maxLineChars - sb.Len(); room > 0 {
			if len(chunk) > room {
				chunk = chunk[:room]
				sb.Write(chunk)
				sb.WriteString("... [line truncated]")
			} else {
				sb.Write(chunk)
			}
		}
		if !more {
			return sb.String(), nil
		}
	}
}
