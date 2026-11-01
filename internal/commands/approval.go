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

// shownCalls holds approved call IDs, so displayTool doesn't repeat what the approver printed.
// Tool calls run one at a time, so it needs no lock.
type shownCalls map[string]bool

// newApprover asks on lines for every call it is given; durable.ReadOnlyGate keeps read-only tools away from it.
func newApprover(lines <-chan string, out io.Writer, shown shownCalls) func(context.Context, durable.ToolCall) (bool, error) {
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
			fmt.Fprintf(out, "\n[%s] %s\n", call.Name, truncate(string(call.Input), 200))
		}
		fmt.Fprint(out, "Run this call? [y/N] (Ctrl-C stops the task) ")

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
				shown[call.ID] = true
				return true, nil
			}
			return false, nil
		}
	}
}
