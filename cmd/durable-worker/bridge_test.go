package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/mcp"
)

func TestMCPHTTPBridgeListsAndCallsTools(t *testing.T) {
	isolate(t)
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "remote", Version: "1"}, nil)
	var seen string
	srv.AddTool(&mcpsdk.Tool{Name: "echo", Description: "echoes the token header", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "token=" + seen}}}, nil
		})
	h := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Token")
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(remote.Close)

	dial := func(_ context.Context, _ string, s mcp.Server) (mcpsdk.Transport, func(), error) {
		cmd := exec.Command(binPath, "mcp-http", "--url", s.URL)
		cmd.Env = append(os.Environ(), headersEnv+`={"X-Token":"s3cret"}`)
		return &mcpsdk.CommandTransport{Command: cmd}, func() {}, nil
	}
	pool := mcp.NewPool(map[string]mcp.Server{"r": {Type: "http", URL: remote.URL}}, mcp.Options{Dial: dial})
	t.Cleanup(func() { require.NoError(t, pool.Close()) })

	pinned, warnings := pool.Discover(t.Context())
	require.Empty(t, warnings)
	require.Len(t, pinned, 1)
	require.Equal(t, "mcp__r__echo", pinned[0].Spec.Name)

	tools := pool.Tools(pinned)
	out, err := tools[0].Execute(t.Context(), durable.ToolCall{ID: "c", Name: "mcp__r__echo", Input: []byte(`{}`)})
	require.NoError(t, err)
	require.Contains(t, out, "token=s3cret")
}

func TestMCPHTTPBridgeRejectsBadHeaders(t *testing.T) {
	isolate(t)
	cmd := exec.CommandContext(t.Context(), binPath, "mcp-http", "--url", "http://127.0.0.1:1/")
	cmd.Env = append(os.Environ(), headersEnv+"=not json")
	out, err := cmd.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(out), headersEnv)
}
