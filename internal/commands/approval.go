package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/wolfeidau/durable"
)

// newApprover asks on lines for mutating tools; read-only tools never ask.
func newApprover(lines <-chan string, out io.Writer) func(context.Context, durable.ToolCall) (bool, error) {
	return func(ctx context.Context, call durable.ToolCall) (bool, error) {
		var in callInput
		_ = json.Unmarshal(call.Input, &in)
		switch call.Name {
		case "bash":
			fmt.Fprintf(out, "\n[bash] %s\n", in.Command)
		case "edit_file":
			if in.OldString == "" {
				fmt.Fprintf(out, "\n[edit_file] create %s\n  new: %s\n", in.Path, truncate(in.NewString, 200))
			} else {
				fmt.Fprintf(out, "\n[edit_file] %s\n  old: %s\n  new: %s\n", in.Path, truncate(in.OldString, 200), truncate(in.NewString, 200))
			}
		default:
			return true, nil
		}
		fmt.Fprint(out, "Run this call? [y/N] ")

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case line, ok := <-lines:
			if !ok {
				// Leaving the call pending is safer than guessing an answer.
				return false, errors.New("stdin closed while waiting for approval")
			}
			switch strings.ToLower(strings.TrimSpace(line)) {
			case "y", "yes":
				return true, nil
			}
			return false, nil
		}
	}
}
