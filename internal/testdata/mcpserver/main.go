// Command mcpserver is a stdio MCP server for the sandbox gateway tests. It is test-only and is not shipped.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	s := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "mcptest", Version: "1"}, nil)
	s.AddTool(&mcpsdk.Tool{Name: "where", Description: "Reports the uid and working directory of this server.", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			cwd, _ := os.Getwd()
			text := fmt.Sprintf("uid=%d cwd=%s marker=%s", os.Getuid(), cwd, os.Getenv("MCPTEST_MARKER"))
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}}}, nil
		})
	if err := s.Run(context.Background(), &mcpsdk.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
