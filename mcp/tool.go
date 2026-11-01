package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/tools"
)

const maxResultBytes = 30 * 1024

// Tools wraps pinned tools as durable.Tool in the same order. It does not connect.
//
// The tools implement neither ReadOnlyTool nor Recoverer: hints from a server
// are only its claim, and nothing can be stopped after a crash.
func (p *Pool) Tools(pinned []Pinned) []durable.Tool {
	out := make([]durable.Tool, len(pinned))
	for i, pt := range pinned {
		out[i] = &mcpTool{pool: p, pinned: pt}
	}
	return out
}

type mcpTool struct {
	pool   *Pool
	pinned Pinned
}

func (t *mcpTool) Spec() durable.ToolSpec { return t.pinned.Spec }

// Replay ignores idempotentHint for the same reason as above.
func (t *mcpTool) Replay() durable.Replay { return durable.ReplayNever }

func (t *mcpTool) Execute(ctx context.Context, call durable.ToolCall) (string, error) {
	server, ok := t.pool.servers[t.pinned.Server]
	if !ok {
		return "", fmt.Errorf("mcp server %q is no longer configured; the call did not run", t.pinned.Server)
	}
	cs, err := t.pool.session(ctx, t.pinned.Server)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("%w; the call did not run", err)
	}

	timeout := defaultCallTimeout
	if server.TimeoutSeconds > 0 {
		timeout = time.Duration(server.TimeoutSeconds) * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := call.Input
	if trimmed := bytes.TrimSpace(args); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		args = json.RawMessage("{}")
	}
	res, err := cs.CallTool(cctx, &mcpsdk.CallToolParams{Name: t.pinned.Tool, Arguments: args})
	if err != nil {
		// Parent cancellation means the process is going away; it is not a tool result.
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("mcp tool %q failed: %w; the call may or may not have taken effect, so check the current state before retrying", t.pinned.Tool, err)
	}

	text := render(res)
	if res.IsError {
		if text == "" {
			text = "the tool reported an error without a message"
		}
		return "", errors.New(text)
	}
	return text, nil
}

func render(res *mcpsdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcpsdk.TextContent:
			parts = append(parts, c.Text)
		case *mcpsdk.ImageContent:
			parts = append(parts, fmt.Sprintf("[image omitted: %s, %d bytes]", c.MIMEType, len(c.Data)))
		case *mcpsdk.AudioContent:
			parts = append(parts, fmt.Sprintf("[audio omitted: %s, %d bytes]", c.MIMEType, len(c.Data)))
		case *mcpsdk.ResourceLink:
			parts = append(parts, fmt.Sprintf("[resource link: %s]", c.URI))
		case *mcpsdk.EmbeddedResource:
			switch r := c.Resource; {
			case r == nil:
			case len(r.Blob) > 0:
				parts = append(parts, fmt.Sprintf("[blob omitted: %s, %d bytes]", r.MIMEType, len(r.Blob)))
			default:
				parts = append(parts, r.Text)
			}
		}
	}
	text := strings.Join(parts, "\n")
	if len(res.Content) == 0 && res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			text = string(b)
		}
	}
	// The cut can split a rune.
	return strings.ToValidUTF8(tools.Truncate(text, maxResultBytes), "")
}
