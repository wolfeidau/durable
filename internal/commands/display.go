package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/wolfeidau/durable"
)

// displayTool prints each call before it runs; Spec and Replay come from the wrapped tool.
type displayTool struct {
	durable.Tool
	out io.Writer
}

func withDisplay(tools []durable.Tool, out io.Writer) []durable.Tool {
	wrapped := make([]durable.Tool, len(tools))
	for i, t := range tools {
		wrapped[i] = displayTool{Tool: t, out: out}
	}
	return wrapped
}

func (d displayTool) Execute(ctx context.Context, call durable.ToolCall) (string, error) {
	fmt.Fprintf(d.out, "\n[%s] %s\n", call.Name, summarize(call.Input))
	return d.Tool.Execute(ctx, call)
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
		return s[:n] + "..."
	}
	return s
}
