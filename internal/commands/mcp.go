package commands

import (
	"context"

	"github.com/wolfeidau/durable/mcp"
)

const mcpPinFile = "mcp-tools.json"

// mcpTools is the session's MCP tool list plus what to tell the user about how it was built.
type mcpTools struct {
	Tools    []mcp.Pinned
	Warnings []string
	// Pinned means the list came from an earlier pin rather than fresh discovery.
	Pinned bool
}

// pinMCPTools pins the tool list on first start; existed (session.sqlite predates this run) pins an older session empty.
func pinMCPTools(ctx context.Context, dir string, existed bool, discover func(context.Context) ([]mcp.Pinned, []string)) (mcpTools, error) {
	var warnings []string
	tools, pinned, err := pinJSON(dir, mcpPinFile, "MCP tools", existed, []mcp.Pinned{}, func() ([]mcp.Pinned, error) {
		var found []mcp.Pinned
		found, warnings = discover(ctx)
		if found == nil {
			found = []mcp.Pinned{}
		}
		return found, nil
	})
	if err != nil {
		return mcpTools{}, err
	}
	if pinned {
		// Warnings belong to a discovery whose result was not used.
		warnings = nil
	}
	return mcpTools{Tools: tools, Warnings: warnings, Pinned: pinned}, nil
}
