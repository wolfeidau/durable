package remote

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/wolfeidau/durable/internal/worker"
	"github.com/wolfeidau/durable/tools"
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

func startWorker(t *testing.T, cfg worker.Config) *httptest.Server {
	t.Helper()
	cfg.Token = testToken
	h, err := worker.Handler(cfg)
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(h)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func realTools(t *testing.T) (string, []durable.Tool) {
	t.Helper()
	root := t.TempDir()
	all, err := tools.All(root, t.TempDir())
	require.NoError(t, err)
	return root, all
}

func dial(t *testing.T, url, token string) *Client {
	t.Helper()
	c, err := Dial(t.Context(), url, token)
	require.NoError(t, err)
	return c
}

func TestToolsComeFromTable(t *testing.T) {
	root, all := realTools(t)
	srv := startWorker(t, worker.Config{Tools: all, Root: root})
	got, err := dial(t, srv.URL, testToken).Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, got, len(all))
	for i, tl := range got {
		require.Equal(t, all[i].Spec().Name, tl.Spec().Name)
		require.Equal(t, all[i].Replay(), tl.Replay())
		require.Equal(t, all[i].(durable.ReadOnlyTool).ReadOnly(), tl.(durable.ReadOnlyTool).ReadOnly())
		require.Implements(t, (*durable.Recoverer)(nil), tl)
	}
}

func TestLieIsNeutralised(t *testing.T) {
	root, all := realTools(t)
	srv := startWorker(t, worker.Config{Tools: all, Root: root, Lie: true})
	c := dial(t, srv.URL, testToken)

	// Prove the worker really lies.
	res, err := c.rpc.ListTools(t.Context(), connect.NewRequest(&workerv1.ListToolsRequest{}))
	require.NoError(t, err)
	names := map[string]bool{}
	for _, ti := range res.Msg.Tools {
		names[ti.Name] = true
		if ti.Name == "bash" {
			require.True(t, ti.ReadOnly)
		}
	}
	require.True(t, names["exfiltrate"])

	got, err := c.Tools(t.Context())
	require.NoError(t, err)
	var gotNames []string
	for _, tl := range got {
		gotNames = append(gotNames, tl.Spec().Name)
		if tl.Spec().Name == "bash" {
			require.False(t, tl.(durable.ReadOnlyTool).ReadOnly())
			require.Equal(t, durable.ReplayNever, tl.Replay())
		}
	}
	require.Equal(t, []string{"read_file", "list_files", "grep", "edit_file", "bash"}, gotNames)
}

func TestExecuteAndErrors(t *testing.T) {
	root, all := realTools(t)
	srv := startWorker(t, worker.Config{Tools: all, Root: root})
	got, err := dial(t, srv.URL, testToken).Tools(t.Context())
	require.NoError(t, err)
	bash := got[4]

	out, err := bash.Execute(t.Context(), durable.ToolCall{ID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"echo hi","timeout_seconds":null}`)})
	require.NoError(t, err)
	require.Contains(t, out, "hi")

	_, err = bash.Execute(t.Context(), durable.ToolCall{ID: "c2", Name: "bash", Input: json.RawMessage(`{"command":"exit 3","timeout_seconds":null}`)})
	require.Error(t, err)
}

func TestInvalidUTF8IsError(t *testing.T) {
	bad := fakeTool{name: "bash", run: func(context.Context, durable.ToolCall) (string, error) { return "a\xffb", nil }}
	srv := startWorker(t, worker.Config{Tools: []durable.Tool{bad}})
	got, err := dial(t, srv.URL, testToken).Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 1)
	_, err = got[0].Execute(t.Context(), durable.ToolCall{ID: "u1", Name: "bash", Input: json.RawMessage(`{}`)})
	require.ErrorContains(t, err, "UTF-8")
}

func TestRecoverReturnsStoredResult(t *testing.T) {
	release := make(chan struct{})
	slow := fakeTool{name: "bash", run: func(context.Context, durable.ToolCall) (string, error) {
		<-release
		return "finished", nil
	}}
	srv := startWorker(t, worker.Config{Tools: []durable.Tool{slow}})
	got, err := dial(t, srv.URL, testToken).Tools(t.Context())
	require.NoError(t, err)
	tl := got[0]
	call := durable.ToolCall{ID: "r1", Name: "bash", Input: json.RawMessage(`{}`)}

	// Cancel, as the harness does: a deadline is sent to the worker, which can answer before ctx ends.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err = tl.Execute(ctx, call)
	require.ErrorIs(t, err, context.Canceled)

	require.Contains(t, tl.(durable.Recoverer).Recover(t.Context(), call), "no result")
	close(release)
	require.Eventually(t, func() bool {
		return tl.(durable.Recoverer).Recover(t.Context(), call) == "finished"
	}, 5*time.Second, 20*time.Millisecond)

	// A known call id returns the stored result without re-running.
	out, err := tl.Execute(t.Context(), call)
	require.NoError(t, err)
	require.Equal(t, "finished", out)
}

func TestWrongTokenRejected(t *testing.T) {
	root, all := realTools(t)
	srv := startWorker(t, worker.Config{Tools: all, Root: root})
	_, err := dial(t, srv.URL, "wrong").Tools(t.Context())
	require.Error(t, err)
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
}

func TestEmptyTokenRefused(t *testing.T) {
	_, err := worker.Handler(worker.Config{})
	require.Error(t, err)
}

func bashFor(t *testing.T, url string) durable.Tool {
	t.Helper()
	got, err := dial(t, url, testToken).Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 1)
	return got[0]
}

func TestDeltasArriveInOrderBeforeResult(t *testing.T) {
	release := make(chan struct{})
	tool := fakeTool{name: "bash", run: func(ctx context.Context, _ durable.ToolCall) (string, error) {
		durable.EmitToolOutput(ctx, "a")
		durable.EmitToolOutput(ctx, "b")
		<-release
		durable.EmitToolOutput(ctx, "c")
		return "done", nil
	}}
	srv := startWorker(t, worker.Config{Tools: []durable.Tool{tool}})
	deltas := make(chan string, 8)
	ctx := durable.WithToolOutputSink(t.Context(), func(s string) { deltas <- s })

	type res struct {
		out string
		err error
	}
	resc := make(chan res, 1)
	bash := bashFor(t, srv.URL)
	go func() {
		out, err := bash.Execute(ctx, durable.ToolCall{ID: "d1", Name: "bash", Input: json.RawMessage(`{}`)})
		resc <- res{out, err}
	}()

	// Both early deltas arrive while the tool is still running.
	require.Equal(t, "a", <-deltas)
	require.Equal(t, "b", <-deltas)
	select {
	case <-resc:
		t.Fatal("result arrived before the tool finished")
	default:
	}
	close(release)
	r := <-resc
	require.NoError(t, r.err)
	require.Equal(t, "done", r.out)
	require.Equal(t, "c", <-deltas)
}

func TestNoDeltasWithoutStreamOutput(t *testing.T) {
	var enabled atomic.Bool
	tool := fakeTool{name: "bash", run: func(ctx context.Context, _ durable.ToolCall) (string, error) {
		enabled.Store(durable.ToolOutputEnabled(ctx))
		durable.EmitToolOutput(ctx, "x")
		return "done", nil
	}}
	srv := startWorker(t, worker.Config{Tools: []durable.Tool{tool}})
	bash := bashFor(t, srv.URL)

	out, err := bash.Execute(t.Context(), durable.ToolCall{ID: "n1", Name: "bash", Input: json.RawMessage(`{}`)})
	require.NoError(t, err)
	require.Equal(t, "done", out)
	require.False(t, enabled.Load())

	// WithoutToolOutput also turns streaming off.
	var seen []string
	ctx := durable.WithoutToolOutput(durable.WithToolOutputSink(t.Context(), func(s string) { seen = append(seen, s) }))
	_, err = bash.Execute(ctx, durable.ToolCall{ID: "n2", Name: "bash", Input: json.RawMessage(`{}`)})
	require.NoError(t, err)
	require.Empty(t, seen)
	require.False(t, enabled.Load())
}

func TestCancelStopsToolAndStoresResult(t *testing.T) {
	started := make(chan struct{})
	tool := fakeTool{name: "bash", run: func(ctx context.Context, _ durable.ToolCall) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}}
	srv := startWorker(t, worker.Config{Tools: []durable.Tool{tool}})
	bash := bashFor(t, srv.URL)
	call := durable.ToolCall{ID: "k1", Name: "bash", Input: json.RawMessage(`{}`)}

	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() {
		_, err := bash.Execute(ctx, call)
		errc <- err
	}()
	<-started
	cancel()
	require.ErrorIs(t, <-errc, context.Canceled)

	require.Eventually(t, func() bool {
		return bash.(durable.Recoverer).Recover(t.Context(), call) == "context canceled"
	}, 5*time.Second, 10*time.Millisecond)

	// A repeat Execute returns the stored result without re-running.
	_, err := bash.Execute(t.Context(), call)
	require.ErrorContains(t, err, "context canceled")
}

func TestCancelUnknownOrFinishedCallIsNoOp(t *testing.T) {
	srv := startWorker(t, worker.Config{Tools: []durable.Tool{fakeTool{name: "bash", run: func(context.Context, durable.ToolCall) (string, error) { return "ok", nil }}}})
	c := dial(t, srv.URL, testToken)
	_, err := c.rpc.Cancel(t.Context(), connect.NewRequest(&workerv1.CancelRequest{CallId: "nope"}))
	require.NoError(t, err)

	bash := bashFor(t, srv.URL)
	call := durable.ToolCall{ID: "f1", Name: "bash", Input: json.RawMessage(`{}`)}
	_, err = bash.Execute(t.Context(), call)
	require.NoError(t, err)
	_, err = c.rpc.Cancel(t.Context(), connect.NewRequest(&workerv1.CancelRequest{CallId: "f1"}))
	require.NoError(t, err)
	out, err := bash.Execute(t.Context(), call)
	require.NoError(t, err)
	require.Equal(t, "ok", out)
}

type cancelingWorker struct {
	workerv1connect.UnimplementedToolServiceHandler
	code connect.Code
}

func (cancelingWorker) ListTools(context.Context, *connect.Request[workerv1.ListToolsRequest]) (*connect.Response[workerv1.ListToolsResponse], error) {
	return connect.NewResponse(&workerv1.ListToolsResponse{Tools: []*workerv1.ToolInfo{{Name: "bash", InputSchema: []byte(`{}`)}}}), nil
}

func (w cancelingWorker) Execute(context.Context, *connect.Request[workerv1.ExecuteRequest], *connect.ServerStream[workerv1.ExecuteResponse]) error {
	return connect.NewError(w.code, errors.New("worker deadline"))
}

func TestCanceledCodeIsCancellationOnlyWhenContextEnded(t *testing.T) {
	for _, code := range []connect.Code{connect.CodeCanceled, connect.CodeDeadlineExceeded} {
		_, h := workerv1connect.NewToolServiceHandler(cancelingWorker{code: code})
		srv := httptest.NewUnstartedServer(h)
		srv.Config.Protocols = new(http.Protocols)
		srv.Config.Protocols.SetUnencryptedHTTP2(true)
		srv.Start()
		t.Cleanup(srv.Close)
		bash := bashFor(t, srv.URL)
		call := durable.ToolCall{ID: "x", Name: "bash", Input: json.RawMessage(`{}`)}

		_, err := bash.Execute(t.Context(), call)
		require.Error(t, err)
		require.NotErrorIs(t, err, context.Canceled)
		require.NotErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, code, connect.CodeOf(err))

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = bash.Execute(ctx, call)
		require.ErrorIs(t, err, context.Canceled)
	}
}
