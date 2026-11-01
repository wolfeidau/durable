package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
)

// setup returns the single tool of a server with the given handler.
func setup(t *testing.T, cfg Server, h mcpsdk.ToolHandler) (durable.Tool, *memServer) {
	t.Helper()
	m := newMemServer(nil)
	m.server.AddTool(objectTool("t"), h)
	if cfg.Type == "" {
		cfg = Server{Type: typeStdio, Command: "unused"}
	}
	p := newMemPool(t, map[string]*memServer{"srv": m}, map[string]Server{"srv": cfg})
	pinned, warnings := discover(t, p)
	require.Empty(t, warnings)
	return p.Tools(pinned)[0], m
}

func echoArgs(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	return text(string(req.Params.Arguments)), nil
}

func TestExecuteReturnsText(t *testing.T) {
	tl, _ := setup(t, Server{}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "one"}, &mcpsdk.TextContent{Text: "two"}}}, nil
	})
	out, err := tl.Execute(context.Background(), durable.ToolCall{Input: json.RawMessage(`{}`)})
	require.NoError(t, err)
	require.Equal(t, "one\ntwo", out)
}

func TestExecuteInputAlwaysReachesServerAsObject(t *testing.T) {
	tl, _ := setup(t, Server{}, echoArgs)
	for name, in := range map[string]json.RawMessage{
		"nil":    nil,
		"empty":  json.RawMessage(""),
		"spaces": json.RawMessage("  "),
		"null":   json.RawMessage("null"),
		"object": json.RawMessage("{}"),
	} {
		out, err := tl.Execute(context.Background(), durable.ToolCall{Input: in})
		require.NoError(t, err, name)
		var obj map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &obj), name)
		require.Empty(t, obj, name)
	}

	out, err := tl.Execute(context.Background(), durable.ToolCall{Input: json.RawMessage(`{"a":1}`)})
	require.NoError(t, err)
	require.JSONEq(t, `{"a":1}`, out)
}

func TestExecuteIsErrorBecomesError(t *testing.T) {
	tl, _ := setup(t, Server{}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		r := text("boom")
		r.IsError = true
		return r, nil
	})
	_, err := tl.Execute(context.Background(), durable.ToolCall{})
	require.EqualError(t, err, "boom")
}

func TestExecuteNonTextContent(t *testing.T) {
	tl, _ := setup(t, Server{}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: "before"},
			&mcpsdk.ImageContent{Data: []byte("12345"), MIMEType: "image/png"},
			&mcpsdk.AudioContent{Data: []byte("123"), MIMEType: "audio/wav"},
			&mcpsdk.EmbeddedResource{Resource: &mcpsdk.ResourceContents{URI: "file:///a", Text: "embedded"}},
			&mcpsdk.EmbeddedResource{Resource: &mcpsdk.ResourceContents{URI: "file:///b", MIMEType: "application/zip", Blob: []byte("1234567")}},
		}}, nil
	})
	out, err := tl.Execute(context.Background(), durable.ToolCall{})
	require.NoError(t, err)
	require.Equal(t, "before\n[image omitted: image/png, 5 bytes]\n[audio omitted: audio/wav, 3 bytes]\nembedded\n[blob omitted: application/zip, 7 bytes]", out)
}

func TestExecuteStructuredContentOnlyWhenContentEmpty(t *testing.T) {
	tl, _ := setup(t, Server{}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{}, StructuredContent: map[string]any{"n": 1}}, nil
	})
	out, err := tl.Execute(context.Background(), durable.ToolCall{})
	require.NoError(t, err)
	require.JSONEq(t, `{"n":1}`, out)

	tl, _ = setup(t, Server{}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		r := text("plain")
		r.StructuredContent = map[string]any{"n": 1}
		return r, nil
	})
	out, err = tl.Execute(context.Background(), durable.ToolCall{})
	require.NoError(t, err)
	require.Equal(t, "plain", out)
}

func TestExecuteTruncatesOnValidUTF8(t *testing.T) {
	tl, _ := setup(t, Server{}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return text(strings.Repeat("€", 40000)), nil
	})
	out, err := tl.Execute(context.Background(), durable.ToolCall{})
	require.NoError(t, err)
	require.Contains(t, out, "bytes omitted")
	require.Less(t, len(out), 31*1024)
	require.Equal(t, strings.ToValidUTF8(out, "�"), out)
}

func TestExecuteTimeoutMayOrMayNotHaveRun(t *testing.T) {
	tl, _ := setup(t, Server{Type: typeStdio, Command: "x", TimeoutSeconds: 1}, func(ctx context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	start := time.Now()
	_, err := tl.Execute(context.Background(), durable.ToolCall{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "may or may not have taken effect")
	require.Contains(t, err.Error(), "check the current state")
	require.Less(t, time.Since(start), 10*time.Second)
}

func TestExecuteParentCancelReturnsContextError(t *testing.T) {
	started := make(chan struct{}, 1)
	tl, _ := setup(t, Server{}, func(ctx context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	_, err := tl.Execute(ctx, durable.ToolCall{})
	require.ErrorIs(t, err, context.Canceled)
	require.NotContains(t, err.Error(), "may or may not")
}

func TestExecuteReconnectsAfterConnectionLoss(t *testing.T) {
	tl, m := setup(t, Server{}, echoArgs)
	_, err := tl.Execute(context.Background(), durable.ToolCall{})
	require.NoError(t, err)
	before := m.dialCount()

	m.drop(t)
	require.Eventually(t, func() bool {
		_, err := tl.Execute(context.Background(), durable.ToolCall{})
		return err == nil
	}, 5*time.Second, 20*time.Millisecond)
	require.Greater(t, m.dialCount(), before)
}

func TestExecuteServerRemovedFromConfig(t *testing.T) {
	m := newMemServer(nil)
	m.server.AddTool(objectTool("t"), echoArgs)
	p := newMemPool(t, map[string]*memServer{"srv": m}, nil)
	pinned, _ := discover(t, p)
	spec := pinned[0].Spec

	empty := NewPool(nil, Options{})
	tl := empty.Tools(pinned)[0]
	require.Equal(t, spec, tl.Spec())
	_, err := tl.Execute(context.Background(), durable.ToolCall{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no longer configured")
	require.Contains(t, err.Error(), "did not run")
}

func TestExecuteConnectFailureMeansDidNotRun(t *testing.T) {
	p := NewPool(map[string]Server{"srv": {Type: typeStdio, Command: "/nonexistent/durable-mcp-test"}}, Options{})
	tl := p.Tools([]Pinned{{Server: "srv", Tool: "t", Spec: durable.ToolSpec{Name: "mcp__srv__t"}}})[0]
	_, err := tl.Execute(context.Background(), durable.ToolCall{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "did not run")
}
