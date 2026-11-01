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

// readOnlyTools uses an explicit declaration because ReplaySafe also covers idempotent writes.
func readOnlyTools(tools []durable.Tool) map[string]bool {
	names := map[string]bool{}
	for _, t := range tools {
		if r, ok := t.(durable.ReadOnlyTool); ok && r.ReadOnly() {
			names[t.Spec().Name] = true
		}
	}
	return names
}

// approvedTools lists the tools the approver prints, which are those not read-only.
func approvedTools(tools []durable.Tool) map[string]bool {
	ro := readOnlyTools(tools)
	names := map[string]bool{}
	for _, t := range tools {
		if n := t.Spec().Name; !ro[n] {
			names[n] = true
		}
	}
	return names
}

// newApprover asks on lines for every call it is given; durable.ReadOnlyGate keeps read-only tools away from it.
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
				return true, nil
			}
			return false, nil
		}
	}
}
