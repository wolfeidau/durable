package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// httpTransport talks to an HTTP server, sending headers only to its own origin.
func httpTransport(endpoint string, headers map[string]string) (*mcpsdk.StreamableClientTransport, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to parse url: %w", err)
	}
	client := &http.Client{Transport: headerTripper{headers: headers, scheme: u.Scheme, host: u.Host, next: http.DefaultTransport}}
	return &mcpsdk.StreamableClientTransport{Endpoint: endpoint, HTTPClient: client, DisableStandaloneSSE: true}, nil
}

// RunBridge serves the tools of the HTTP MCP server at endpoint over t, so a stdio client reaches it from where the
// bridge runs. It proxies tools only, because that is all durable uses, and lists them once because they are pinned.
func RunBridge(ctx context.Context, endpoint string, headers map[string]string, t mcpsdk.Transport) error {
	ht, err := httpTransport(endpoint, headers)
	if err != nil {
		return err
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "durable-bridge", Version: "dev"}, &mcpsdk.ClientOptions{Capabilities: &mcpsdk.ClientCapabilities{}})
	cs, err := client.Connect(ctx, ht, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", endpoint, err)
	}
	defer cs.Close()

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "durable-bridge", Version: "dev"}, &mcpsdk.ServerOptions{Capabilities: &mcpsdk.ServerCapabilities{Tools: &mcpsdk.ToolCapabilities{}}})
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return fmt.Errorf("failed to list tools of %s: %w", endpoint, err)
		}
		// AddTool panics on a schema that is not an object; the host would skip such a tool anyway.
		if _, err := inputSchema(tool.InputSchema); err != nil {
			continue
		}
		srv.AddTool(tool, func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: req.Params.Name, Arguments: req.Params.Arguments})
		})
	}
	return srv.Run(ctx, t)
}
