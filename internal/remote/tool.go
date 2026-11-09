// Package remote runs durable tools on a worker inside a sandbox.
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"unicode/utf8"

	"connectrpc.com/connect"

	"github.com/wolfeidau/durable"
	workerv1 "github.com/wolfeidau/durable/api/durable/worker/v1"
	"github.com/wolfeidau/durable/api/durable/worker/v1/workerv1connect"
	"github.com/wolfeidau/durable/tools"
)

// maxOutputBytes matches the bash tool's cap.
const maxOutputBytes = 30 * 1024

// known mirrors tools.All; worker-claimed read_only and replay are never trusted.
var known = []struct {
	name     string
	readOnly bool
	replay   durable.Replay
}{
	{"read_file", true, durable.ReplaySafe},
	{"list_files", true, durable.ReplaySafe},
	{"grep", true, durable.ReplaySafe},
	{"edit_file", false, durable.ReplayNever},
	{"bash", false, durable.ReplayNever},
}

// Client talks to one worker.
type Client struct {
	rpc workerv1connect.ToolServiceClient
}

// Dial returns a client for the worker at baseURL. It makes no request.
func Dial(_ context.Context, baseURL, token string) (*Client, error) {
	transport := &http.Transport{Protocols: new(http.Protocols)}
	transport.Protocols.SetUnencryptedHTTP2(true)
	auth := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	})
	return &Client{rpc: workerv1connect.NewToolServiceClient(&http.Client{Transport: transport}, baseURL, connect.WithInterceptors(auth))}, nil
}

// Info returns the worker's view of its environment.
func (c *Client) Info(ctx context.Context) (*workerv1.InfoResponse, error) {
	res, err := c.rpc.Info(ctx, connect.NewRequest(&workerv1.InfoRequest{}))
	if err != nil {
		return nil, fmt.Errorf("failed to get worker info: %w", err)
	}
	return res.Msg, nil
}

// Result returns the stored result for a call, if the worker has one.
func (c *Client) Result(ctx context.Context, callID string) (*workerv1.ResultResponse, error) {
	res, err := c.rpc.Result(ctx, connect.NewRequest(&workerv1.ResultRequest{CallId: callID}))
	if err != nil {
		return nil, fmt.Errorf("failed to get result: %w", err)
	}
	return res.Msg, nil
}

// RawTools returns exactly what the worker claims, for the spike to show the claims are ignored.
func (c *Client) RawTools(ctx context.Context) ([]*workerv1.ToolInfo, error) {
	res, err := c.rpc.ListTools(ctx, connect.NewRequest(&workerv1.ListToolsRequest{}))
	if err != nil {
		return nil, fmt.Errorf("failed to list tools: %w", err)
	}
	return res.Msg.Tools, nil
}

// Tools returns the known tools in fixed order; the worker supplies only name, description and schema.
func (c *Client) Tools(ctx context.Context) ([]durable.Tool, error) {
	res, err := c.rpc.ListTools(ctx, connect.NewRequest(&workerv1.ListToolsRequest{}))
	if err != nil {
		return nil, fmt.Errorf("failed to list tools: %w", err)
	}
	byName := map[string]*workerv1.ToolInfo{}
	for _, t := range res.Msg.Tools {
		byName[t.Name] = t
	}
	var out []durable.Tool
	for _, k := range known {
		info, ok := byName[k.name]
		if !ok {
			continue
		}
		out = append(out, &remoteTool{
			c:        c,
			spec:     durable.ToolSpec{Name: k.name, Description: info.Description, Schema: json.RawMessage(info.InputSchema)},
			readOnly: k.readOnly,
			replay:   k.replay,
		})
	}
	return out, nil
}

type remoteTool struct {
	c        *Client
	spec     durable.ToolSpec
	readOnly bool
	replay   durable.Replay
}

var (
	_ durable.Tool         = (*remoteTool)(nil)
	_ durable.ReadOnlyTool = (*remoteTool)(nil)
	_ durable.Recoverer    = (*remoteTool)(nil)
)

func (t *remoteTool) Spec() durable.ToolSpec { return t.spec }
func (t *remoteTool) Replay() durable.Replay { return t.replay }
func (t *remoteTool) ReadOnly() bool         { return t.readOnly }

func (t *remoteTool) Execute(ctx context.Context, call durable.ToolCall) (string, error) {
	res, err := t.c.rpc.Execute(ctx, connect.NewRequest(&workerv1.ExecuteRequest{CallId: call.ID, Name: call.Name, Input: call.Input}))
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("failed to execute %s on worker: %w", call.Name, err)
	}
	out, err := clean(res.Msg.Output)
	if err != nil {
		return "", err
	}
	if res.Msg.IsError {
		return "", errors.New(out)
	}
	return out, nil
}

func (t *remoteTool) Recover(ctx context.Context, call durable.ToolCall) string {
	res, err := t.c.Result(ctx, call.ID)
	if err != nil {
		return "The worker could not be reached to check this call: " + err.Error()
	}
	if !res.Found {
		return "The worker has no result for this call; it may not have finished or the worker restarted."
	}
	out, err := clean(res.Output)
	if err != nil {
		return err.Error()
	}
	return out
}

func clean(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("worker returned output that is not valid UTF-8")
	}
	return tools.Truncate(s, maxOutputBytes), nil
}
