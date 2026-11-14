package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

const mcpServerEnv = "DURABLE_COMMANDS_MCP_SERVER"

// TestMain lets the test binary act as a stdio MCP server, which the worker then starts.
func TestMain(m *testing.M) {
	if os.Getenv(mcpServerEnv) == "1" {
		s := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "where", Version: "1"}, nil)
		s.AddTool(&mcpsdk.Tool{Name: "where", Description: "reports the working directory", InputSchema: map[string]any{"type": "object"}},
			func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				cwd, _ := os.Getwd()
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: fmt.Sprintf("cwd=%s uid=%d", cwd, os.Getuid())}}}, nil
			})
		_ = s.Run(context.Background(), &mcpsdk.StdioTransport{})
		return
	}
	// A direct Anthropic session asks OpenRouter's list, so tests point it at a closed port.
	_ = os.Setenv("OPENROUTER_BASE_URL", "http://127.0.0.1:1")
	os.Exit(m.Run())
}

// writeStdioMCP configures the re-executed test binary as the MCP server "m".
func writeStdioMCP(t *testing.T) {
	t.Helper()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable"), 0o700))
	cfg := fmt.Sprintf(`{"mcpServers":{"m":{"command":%q,"env":{%q:"1"},"timeoutSeconds":10}}}`, os.Args[0], mcpServerEnv)
	require.NoError(t, os.WriteFile(filepath.Join(home, ".durable", "mcp.json"), []byte(cfg), 0o600))
}
