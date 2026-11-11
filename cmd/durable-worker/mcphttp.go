package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wolfeidau/durable/mcp"
)

// headersEnv carries the headers as a JSON object, so they never appear in the process list.
const headersEnv = "DURABLE_MCP_HEADERS"

type mcpHTTPCmd struct {
	URL string `help:"URL of the HTTP MCP server." required:""`
}

func (c *mcpHTTPCmd) Run(ctx context.Context) error {
	var headers map[string]string
	if raw := os.Getenv(headersEnv); raw != "" {
		if err := json.Unmarshal([]byte(raw), &headers); err != nil {
			return fmt.Errorf("failed to parse %s: %w", headersEnv, err)
		}
	}
	// Children don't inherit the headers; /proc/<pid>/environ still shows them, which the docs accept for now.
	if err := os.Unsetenv(headersEnv); err != nil {
		return fmt.Errorf("failed to unset %s: %w", headersEnv, err)
	}
	return mcp.RunBridge(ctx, c.URL, headers, &mcpsdk.StdioTransport{})
}
