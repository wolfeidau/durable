package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/wolfeidau/durable"
)

var _ durable.ReadOnlyTool = displayTool{}

// displayTool prints each call before it runs; Spec and Replay come from the wrapped tool.
type displayTool struct {
	durable.Tool
	out   io.Writer
	quiet bool
}

// withDisplay skips the header for tools in quiet, because the approver already shows them.
func withDisplay(tools []durable.Tool, out io.Writer, quiet map[string]bool) []durable.Tool {
	wrapped := make([]durable.Tool, len(tools))
	for i, t := range tools {
		wrapped[i] = displayTool{Tool: t, out: out, quiet: quiet[t.Spec().Name]}
	}
	return wrapped
}

func (d displayTool) Execute(ctx context.Context, call durable.ToolCall) (string, error) {
	if !d.quiet {
		fmt.Fprintf(d.out, "\n[%s] %s\n", call.Name, summarize(call.Input))
	}
	return d.Tool.Execute(ctx, call)
}

// Recover forwards to the wrapped tool; the embedded interface does not promote optional methods.
func (d displayTool) Recover(ctx context.Context, call durable.ToolCall) string {
	if r, ok := d.Tool.(durable.Recoverer); ok {
		return r.Recover(ctx, call)
	}
	return ""
}

// ReadOnly forwards to the wrapped tool for the same reason as Recover.
func (d displayTool) ReadOnly() bool {
	r, ok := d.Tool.(durable.ReadOnlyTool)
	return ok && r.ReadOnly()
}

type callInput struct {
	Path      string `json:"path"`
	Command   string `json:"command"`
	Pattern   string `json:"pattern"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

func summarize(input json.RawMessage) string {
	var in callInput
	if err := json.Unmarshal(input, &in); err != nil {
		return truncate(string(input), 100)
	}
	switch {
	case in.Command != "":
		return truncate(in.Command, 100)
	case in.Pattern != "":
		return truncate(strings.TrimSpace(in.Pattern+" "+in.Path), 100)
	case in.Path != "":
		return truncate(in.Path, 100)
	}
	return truncate(string(input), 100)
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return strings.ToValidUTF8(s[:n], "") + "..."
	}
	return s
}
