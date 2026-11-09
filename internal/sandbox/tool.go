package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

	"github.com/wolfeidau/durable"
	workerv1 "github.com/wolfeidau/durable/api/durable/worker/v1"
	"github.com/wolfeidau/durable/tools"
)

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
	req := &workerv1.ExecuteRequest{CallId: call.ID, Name: call.Name, Input: call.Input, StreamOutput: durable.ToolOutputEnabled(ctx)}
	stream, err := t.c.rpc.Execute(ctx, connect.NewRequest(req))
	if err != nil {
		return "", t.failed(ctx, call, err)
	}
	defer stream.Close()
	var res *workerv1.ExecuteResult
	for stream.Receive() {
		switch ev := stream.Msg().Event.(type) {
		case *workerv1.ExecuteResponse_Delta:
			if utf8.ValidString(ev.Delta) {
				durable.EmitToolOutput(ctx, ev.Delta)
			}
		case *workerv1.ExecuteResponse_Result:
			res = ev.Result
		}
	}
	if err := stream.Err(); err != nil {
		return "", t.failed(ctx, call, err)
	}
	if res == nil {
		return "", t.failed(ctx, call, errors.New("stream ended without a result"))
	}
	out, err := clean(res.Output)
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", errors.New(out)
	}
	return out, nil
}

// failed maps an RPC failure. A canceled or deadline_exceeded code counts as cancellation only when ctx has
// ended, because Connect also reports those codes for the worker's own deadlines.
func (t *remoteTool) failed(ctx context.Context, call durable.ToolCall, err error) error {
	if ctx.Err() != nil {
		// The worker keeps the call running after a dropped request, so stop it explicitly.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
		defer cancel()
		_, _ = t.c.rpc.Cancel(cctx, connect.NewRequest(&workerv1.CancelRequest{CallId: call.ID})) // best effort; the worker stores the result either way
		return ctx.Err()
	}
	return fmt.Errorf("failed to execute %s on worker: %w", call.Name, err)
}

// Recover cancels the call and returns its stored result, or a note when the worker has none.
func (t *remoteTool) Recover(ctx context.Context, call durable.ToolCall) string {
	pctx, cancel := context.WithTimeout(ctx, t.c.recoverWait)
	defer cancel()
	if _, err := t.c.rpc.Cancel(pctx, connect.NewRequest(&workerv1.CancelRequest{CallId: call.ID})); err != nil {
		return "The worker could not be reached to check this call: " + err.Error()
	}
	tick := time.NewTicker(t.c.recoverEvery)
	defer tick.Stop()
	for {
		res, err := t.c.rpc.Result(pctx, connect.NewRequest(&workerv1.ResultRequest{CallId: call.ID}))
		switch {
		case err == nil && res.Msg.Found:
			out, cerr := clean(res.Msg.Output)
			if cerr != nil {
				return cerr.Error()
			}
			return out
		case err != nil && pctx.Err() == nil:
			return "The worker could not be reached to check this call: " + err.Error()
		}
		select {
		case <-pctx.Done():
			return "The worker has no result for this call; it may not have finished or the worker restarted."
		case <-tick.C:
		}
	}
}

func clean(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("worker returned output that is not valid UTF-8")
	}
	return tools.Truncate(s, tools.MaxOutputBytes), nil
}
