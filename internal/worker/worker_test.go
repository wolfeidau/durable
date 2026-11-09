package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	workerv1 "github.com/wolfeidau/durable/api/durable/worker/v1"
	"github.com/wolfeidau/durable/api/durable/worker/v1/workerv1connect"
)

const testToken = "test-token"

type fakeTool struct {
	name string
	run  func(ctx context.Context, call durable.ToolCall) (string, error)
}

func (f fakeTool) Spec() durable.ToolSpec {
	return durable.ToolSpec{Name: f.name, Description: "fake " + f.name, Schema: json.RawMessage(`{"type":"object"}`)}
}
func (f fakeTool) Replay() durable.Replay { return durable.ReplayNever }
func (f fakeTool) Execute(ctx context.Context, call durable.ToolCall) (string, error) {
	return f.run(ctx, call)
}

func start(t *testing.T, tools ...durable.Tool) workerv1connect.ToolServiceClient {
	t.Helper()
	h, err := Handler(Config{Tools: tools, Root: "/root", Token: testToken})
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(h)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)

	transport := &http.Transport{Protocols: new(http.Protocols)}
	transport.Protocols.SetUnencryptedHTTP2(true)
	t.Cleanup(transport.CloseIdleConnections)
	return workerv1connect.NewToolServiceClient(&http.Client{Transport: transport}, srv.URL)
}

func authed[T any](msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+testToken)
	return req
}

type execResult struct {
	deltas []string
	result *workerv1.ExecuteResult
}

func execute(ctx context.Context, c workerv1connect.ToolServiceClient, m *workerv1.ExecuteRequest) (execResult, error) {
	stream, err := c.Execute(ctx, authed(m))
	if err != nil {
		return execResult{}, err
	}
	defer stream.Close()
	var out execResult
	for stream.Receive() {
		switch ev := stream.Msg().Event.(type) {
		case *workerv1.ExecuteResponse_Delta:
			out.deltas = append(out.deltas, ev.Delta)
		case *workerv1.ExecuteResponse_Result:
			out.result = ev.Result
		}
	}
	return out, stream.Err()
}

func echoTool(name string) fakeTool {
	return fakeTool{name: name, run: func(_ context.Context, call durable.ToolCall) (string, error) {
		return string(call.Input), nil
	}}
}

func TestEmptyTokenRefused(t *testing.T) {
	_, err := Handler(Config{Tools: []durable.Tool{echoTool("echo")}})
	require.Error(t, err)
}

func TestAuthRejected(t *testing.T) {
	c := start(t, echoTool("echo"))

	_, err := c.Info(t.Context(), connect.NewRequest(&workerv1.InfoRequest{}))
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))

	bad := connect.NewRequest(&workerv1.InfoRequest{})
	bad.Header().Set("Authorization", "Bearer wrong")
	_, err = c.Info(t.Context(), bad)
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))

	stream, err := c.Execute(t.Context(), connect.NewRequest(&workerv1.ExecuteRequest{CallId: "1", Name: "echo"}))
	if err == nil {
		defer stream.Close()
		for stream.Receive() {
		}
		err = stream.Err()
	}
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
}

func TestExecuteValidation(t *testing.T) {
	c := start(t, echoTool("echo"))

	_, err := execute(t.Context(), c, &workerv1.ExecuteRequest{Name: "echo"})
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))

	_, err = execute(t.Context(), c, &workerv1.ExecuteRequest{CallId: "1", Name: "nope"})
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
}

func TestExecuteRunsTool(t *testing.T) {
	c := start(t, echoTool("echo"))
	res, err := execute(t.Context(), c, &workerv1.ExecuteRequest{CallId: "1", Name: "echo", Input: []byte("hi")})
	require.NoError(t, err)
	require.Equal(t, "hi", res.result.Output)
	require.False(t, res.result.IsError)
}

func TestToolErrorIsStoredAsError(t *testing.T) {
	c := start(t, fakeTool{name: "fail", run: func(context.Context, durable.ToolCall) (string, error) {
		return "", context.DeadlineExceeded
	}})
	res, err := execute(t.Context(), c, &workerv1.ExecuteRequest{CallId: "1", Name: "fail"})
	require.NoError(t, err)
	require.True(t, res.result.IsError)
	require.Equal(t, context.DeadlineExceeded.Error(), res.result.Output)
}

func TestFinishedCallReturnsStoredResultWithoutRerun(t *testing.T) {
	var runs atomic.Int32
	c := start(t, fakeTool{name: "count", run: func(context.Context, durable.ToolCall) (string, error) {
		return "run " + fmt.Sprint(runs.Add(1)), nil
	}})
	for range 3 {
		res, err := execute(t.Context(), c, &workerv1.ExecuteRequest{CallId: "1", Name: "count"})
		require.NoError(t, err)
		require.Equal(t, "run 1", res.result.Output)
	}
	require.EqualValues(t, 1, runs.Load())
}

// blocker is a tool that signals when it starts and finishes when released or cancelled.
type blocker struct {
	started chan struct{}
	release chan struct{}
	runs    atomic.Int32
}

func newBlocker() *blocker {
	return &blocker{started: make(chan struct{}, 8), release: make(chan struct{})}
}

func (b *blocker) tool() fakeTool {
	return fakeTool{name: "block", run: func(ctx context.Context, _ durable.ToolCall) (string, error) {
		b.runs.Add(1)
		b.started <- struct{}{}
		select {
		case <-b.release:
			return "released", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}}
}

func TestRunningCallWaitsAndGetsResultWithoutDeltas(t *testing.T) {
	b := newBlocker()
	tl := fakeTool{name: "block", run: func(ctx context.Context, call durable.ToolCall) (string, error) {
		durable.EmitToolOutput(ctx, "live")
		return b.tool().run(ctx, call)
	}}
	c := start(t, tl)

	first := make(chan execResult, 1)
	go func() {
		res, _ := execute(t.Context(), c, &workerv1.ExecuteRequest{CallId: "1", Name: "block", StreamOutput: true})
		first <- res
	}()
	<-b.started

	second := make(chan execResult, 1)
	go func() {
		res, _ := execute(t.Context(), c, &workerv1.ExecuteRequest{CallId: "1", Name: "block", StreamOutput: true})
		second <- res
	}()
	// The second request must be attached, not running the tool again.
	time.Sleep(50 * time.Millisecond)
	close(b.release)

	got := <-second
	require.Equal(t, "released", got.result.Output)
	require.Empty(t, got.deltas)
	require.Equal(t, []string{"live"}, (<-first).deltas)
	require.EqualValues(t, 1, b.runs.Load())
}

func TestDeltasOnlyWithStreamOutput(t *testing.T) {
	tl := fakeTool{name: "chatty", run: func(ctx context.Context, _ durable.ToolCall) (string, error) {
		durable.EmitToolOutput(ctx, "a")
		durable.EmitToolOutput(ctx, "b")
		return "done", nil
	}}
	c := start(t, tl)

	quiet, err := execute(t.Context(), c, &workerv1.ExecuteRequest{CallId: "quiet", Name: "chatty"})
	require.NoError(t, err)
	require.Empty(t, quiet.deltas)
	require.Equal(t, "done", quiet.result.Output)

	loud, err := execute(t.Context(), c, &workerv1.ExecuteRequest{CallId: "loud", Name: "chatty", StreamOutput: true})
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, loud.deltas)

	again, err := execute(t.Context(), c, &workerv1.ExecuteRequest{CallId: "loud", Name: "chatty", StreamOutput: true})
	require.NoError(t, err)
	require.Empty(t, again.deltas)
	require.Equal(t, "done", again.result.Output)
}

func TestSlowReaderNeverStallsTool(t *testing.T) {
	tl := fakeTool{name: "flood", run: func(ctx context.Context, _ durable.ToolCall) (string, error) {
		for range 10 * deltaBuffer {
			durable.EmitToolOutput(ctx, "x")
		}
		return "done", nil
	}}
	c := start(t, tl)
	res, err := execute(t.Context(), c, &workerv1.ExecuteRequest{CallId: "1", Name: "flood", StreamOutput: true})
	require.NoError(t, err)
	require.Equal(t, "done", res.result.Output)
	require.LessOrEqual(t, len(res.deltas), 10*deltaBuffer)
}

func TestCancelStopsCallAndStoresResult(t *testing.T) {
	b := newBlocker()
	c := start(t, b.tool())

	done := make(chan execResult, 1)
	go func() {
		res, _ := execute(t.Context(), c, &workerv1.ExecuteRequest{CallId: "1", Name: "block"})
		done <- res
	}()
	<-b.started

	_, err := c.Cancel(t.Context(), authed(&workerv1.CancelRequest{CallId: "1"}))
	require.NoError(t, err)
	res := <-done
	require.True(t, res.result.IsError)
	require.Equal(t, context.Canceled.Error(), res.result.Output)

	stored, err := c.Result(t.Context(), authed(&workerv1.ResultRequest{CallId: "1"}))
	require.NoError(t, err)
	require.True(t, stored.Msg.Found)
	require.True(t, stored.Msg.IsError)

	// Unknown and finished calls are no-ops.
	for _, id := range []string{"1", "unknown"} {
		_, err = c.Cancel(t.Context(), authed(&workerv1.CancelRequest{CallId: id}))
		require.NoError(t, err)
	}
}

func TestDroppedRequestDoesNotStopCall(t *testing.T) {
	b := newBlocker()
	c := start(t, b.tool())

	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() {
		_, err := execute(ctx, c, &workerv1.ExecuteRequest{CallId: "1", Name: "block"})
		errc <- err
	}()
	<-b.started
	cancel()
	require.Equal(t, connect.CodeCanceled, connect.CodeOf(<-errc))

	res, err := c.Result(t.Context(), authed(&workerv1.ResultRequest{CallId: "1"}))
	require.NoError(t, err)
	require.False(t, res.Msg.Found, "call must still be running")

	close(b.release)
	got, err := execute(t.Context(), c, &workerv1.ExecuteRequest{CallId: "1", Name: "block"})
	require.NoError(t, err)
	require.Equal(t, "released", got.result.Output)
	require.EqualValues(t, 1, b.runs.Load())
}

func TestResultUnknownNotFound(t *testing.T) {
	c := start(t, echoTool("echo"))
	res, err := c.Result(t.Context(), authed(&workerv1.ResultRequest{CallId: "nope"}))
	require.NoError(t, err)
	require.False(t, res.Msg.Found)
}

func TestListTools(t *testing.T) {
	c := start(t, echoTool("a"), echoTool("b"))
	res, err := c.ListTools(t.Context(), authed(&workerv1.ListToolsRequest{}))
	require.NoError(t, err)
	require.Len(t, res.Msg.Tools, 2)
	require.Equal(t, "a", res.Msg.Tools[0].Name)
	require.Equal(t, "fake b", res.Msg.Tools[1].Description)
	require.JSONEq(t, `{"type":"object"}`, string(res.Msg.Tools[1].InputSchema))
}

func TestInfoHidesToken(t *testing.T) {
	t.Setenv(TokenEnv, "secret-value")
	t.Setenv("DURABLE_WORKER_TEST_VISIBLE", "yes")
	c := start(t, echoTool("echo"))
	res, err := c.Info(t.Context(), authed(&workerv1.InfoRequest{}))
	require.NoError(t, err)
	require.NotContains(t, res.Msg.Env, TokenEnv)
	require.Equal(t, "yes", res.Msg.Env["DURABLE_WORKER_TEST_VISIBLE"])
	require.Equal(t, "/root", res.Msg.Root)
	require.NotEmpty(t, res.Msg.Cwd)
}
