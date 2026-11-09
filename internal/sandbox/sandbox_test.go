package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	spec durable.ToolSpec
	run  func(ctx context.Context, call durable.ToolCall) (string, error)
}

func (f fakeTool) Spec() durable.ToolSpec { return f.spec }
func (f fakeTool) Replay() durable.Replay { return durable.ReplayNever }
func (f fakeTool) Execute(ctx context.Context, call durable.ToolCall) (string, error) {
	return f.run(ctx, call)
}

func fake(name string, run func(ctx context.Context, call durable.ToolCall) (string, error)) fakeTool {
	return fakeTool{spec: durable.ToolSpec{Name: name, Description: "fake", Schema: json.RawMessage(`{"type":"object"}`)}, run: run}
}

// respec reports different metadata for a real tool.
type respec struct {
	durable.Tool
	spec durable.ToolSpec
}

func (r respec) Spec() durable.ToolSpec { return r.spec }

func serve(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func startWorker(t *testing.T, ts ...durable.Tool) *Client {
	t.Helper()
	h, err := worker.Handler(worker.Config{Tools: ts, Root: "/root", Token: testToken})
	require.NoError(t, err)
	c := NewClient(serve(t, h).URL, testToken, nil)
	t.Cleanup(c.CloseIdle)
	return c
}

func realTools(t *testing.T) []durable.Tool {
	t.Helper()
	all, err := tools.All(t.TempDir(), t.TempDir())
	require.NoError(t, err)
	return all
}

// remote builds a tool for a fake worker tool, bypassing the catalog check.
func remote(c *Client, name string) *remoteTool {
	return &remoteTool{c: c, spec: durable.ToolSpec{Name: name}, replay: durable.ReplayNever}
}

func TestToolsUseCatalogMetadata(t *testing.T) {
	got, err := startWorker(t, realTools(t)...).Tools(t.Context())
	require.NoError(t, err)
	cat := tools.Catalog()
	require.Len(t, got, len(cat))
	for i, tl := range got {
		require.Equal(t, cat[i].Spec, tl.Spec())
		require.Equal(t, cat[i].Replay, tl.Replay())
		require.Equal(t, cat[i].ReadOnly, tl.(durable.ReadOnlyTool).ReadOnly())
		require.Implements(t, (*durable.Recoverer)(nil), tl)
	}
}

func TestToolsAcceptReformattedSchema(t *testing.T) {
	all := realTools(t)
	var pretty json.RawMessage
	var v any
	require.NoError(t, json.Unmarshal(all[0].Spec().Schema, &v))
	pretty, err := json.MarshalIndent(v, "", "  ")
	require.NoError(t, err)
	spec := all[0].Spec()
	spec.Schema = pretty
	all[0] = respec{Tool: all[0], spec: spec}

	_, err = startWorker(t, all...).Tools(t.Context())
	require.NoError(t, err)
}

func TestToolsRejectMismatch(t *testing.T) {
	changed := func(f func(*durable.ToolSpec)) []durable.Tool {
		all := realTools(t)
		spec := all[4].Spec()
		f(&spec)
		all[4] = respec{Tool: all[4], spec: spec}
		return all
	}
	extra := append(realTools(t), fake("exfiltrate", nil))
	reordered := realTools(t)
	reordered[0], reordered[1] = reordered[1], reordered[0]

	tests := map[string]struct {
		tools []durable.Tool
		want  string
	}{
		"description": {changed(func(s *durable.ToolSpec) { s.Description = "harmless" }), "description"},
		"schema":      {changed(func(s *durable.ToolSpec) { s.Schema = json.RawMessage(`{"type":"object"}`) }), "schema"},
		"missing":     {realTools(t)[:4], "offers 4 tools"},
		"extra":       {extra, "offers 6 tools"},
		"reordered":   {reordered, `is "list_files", want "read_file"`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := startWorker(t, tc.tools...).Tools(t.Context())
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestWrongTokenRejected(t *testing.T) {
	h, err := worker.Handler(worker.Config{Tools: realTools(t), Token: testToken})
	require.NoError(t, err)
	c := NewClient(serve(t, h).URL, "wrong", nil)
	t.Cleanup(c.CloseIdle)
	_, err = c.Tools(t.Context())
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
	_, err = remote(c, "x").Execute(t.Context(), durable.ToolCall{ID: "1", Name: "x"})
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
}

func TestDeltasArriveInOrderBeforeResult(t *testing.T) {
	c := startWorker(t, fake("chatty", func(ctx context.Context, _ durable.ToolCall) (string, error) {
		for _, s := range []string{"1", "2", "3"} {
			durable.EmitToolOutput(ctx, s)
		}
		return "done", nil
	}))
	var got []string
	ctx := durable.WithToolOutputSink(t.Context(), func(s string) { got = append(got, s) })
	out, err := remote(c, "chatty").Execute(ctx, durable.ToolCall{ID: "1", Name: "chatty"})
	require.NoError(t, err)
	require.Equal(t, "done", out)
	require.Equal(t, []string{"1", "2", "3"}, got)
}

func TestNoDeltasWithoutStreamOutput(t *testing.T) {
	seen := make(chan bool, 1)
	c := startWorker(t, fake("probe", func(ctx context.Context, _ durable.ToolCall) (string, error) {
		seen <- durable.ToolOutputEnabled(ctx)
		return "ok", nil
	}))
	out, err := remote(c, "probe").Execute(t.Context(), durable.ToolCall{ID: "1", Name: "probe"})
	require.NoError(t, err)
	require.Equal(t, "ok", out)
	require.False(t, <-seen)

	ctx := durable.WithoutToolOutput(durable.WithToolOutputSink(t.Context(), func(string) {}))
	_, err = remote(c, "probe").Execute(ctx, durable.ToolCall{ID: "2", Name: "probe"})
	require.NoError(t, err)
	require.False(t, <-seen)
}

func TestInvalidUTF8Rejected(t *testing.T) {
	c := startWorker(t, fake("bad", func(ctx context.Context, _ durable.ToolCall) (string, error) {
		durable.EmitToolOutput(ctx, "ok")
		return "\xff\xfe", nil
	}))
	_, err := remote(c, "bad").Execute(t.Context(), durable.ToolCall{ID: "1", Name: "bad"})
	require.ErrorContains(t, err, "UTF-8")
}

func TestOutputCapped(t *testing.T) {
	c := startWorker(t, fake("big", func(context.Context, durable.ToolCall) (string, error) {
		return strings.Repeat("a", 10*tools.MaxOutputBytes), nil
	}))
	out, err := remote(c, "big").Execute(t.Context(), durable.ToolCall{ID: "1", Name: "big"})
	require.NoError(t, err)
	require.Less(t, len(out), tools.MaxOutputBytes+100)
	require.Contains(t, out, "bytes omitted")
}

func TestWorkerErrorIsToolError(t *testing.T) {
	c := startWorker(t, fake("fail", func(context.Context, durable.ToolCall) (string, error) {
		return "", errors.New("boom")
	}))
	out, err := remote(c, "fail").Execute(t.Context(), durable.ToolCall{ID: "1", Name: "fail"})
	require.EqualError(t, err, "boom")
	require.Empty(t, out)
}

// blocking runs until its context ends, then reports partial output.
type blocking struct {
	started   chan struct{}
	cancelled chan struct{}
}

func newBlocking() *blocking {
	return &blocking{started: make(chan struct{}), cancelled: make(chan struct{})}
}

func (b *blocking) tool() fakeTool {
	var once sync.Once
	return fake("block", func(ctx context.Context, _ durable.ToolCall) (string, error) {
		once.Do(func() { close(b.started) })
		<-ctx.Done()
		close(b.cancelled)
		return "partial", nil
	})
}

func TestContextCancelSendsCancelAndReturnsCtxErr(t *testing.T) {
	b := newBlocking()
	c := startWorker(t, b.tool())

	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() {
		_, err := remote(c, "block").Execute(ctx, durable.ToolCall{ID: "1", Name: "block"})
		errc <- err
	}()
	<-b.started
	cancel()

	require.ErrorIs(t, <-errc, context.Canceled)
	select {
	case <-b.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("worker was never told to cancel the call")
	}
}

func TestRecoverReturnsStoredResultAfterCancel(t *testing.T) {
	b := newBlocking()
	c := startWorker(t, b.tool())
	tl := remote(c, "block")

	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() {
		_, err := tl.Execute(ctx, durable.ToolCall{ID: "1", Name: "block"})
		errc <- err
	}()
	<-b.started
	cancel()
	require.ErrorIs(t, <-errc, context.Canceled)

	for range 2 {
		require.Equal(t, "partial", tl.Recover(t.Context(), durable.ToolCall{ID: "1", Name: "block"}))
	}
}

func TestRecoverCancelsRunningCall(t *testing.T) {
	b := newBlocking()
	c := startWorker(t, b.tool())
	tl := remote(c, "block")

	go func() { _, _ = tl.Execute(t.Context(), durable.ToolCall{ID: "1", Name: "block"}) }()
	<-b.started
	require.Equal(t, "partial", tl.Recover(t.Context(), durable.ToolCall{ID: "1", Name: "block"}))
}

func TestRecoverUnknownCall(t *testing.T) {
	c := startWorker(t, fake("echo", nil))
	c.recoverWait, c.recoverEvery = 200*time.Millisecond, 20*time.Millisecond
	note := remote(c, "echo").Recover(t.Context(), durable.ToolCall{ID: "nope", Name: "echo"})
	require.Contains(t, note, "no result")
}

// stub overrides Execute on a worker that is otherwise unimplemented.
type stub struct {
	workerv1connect.UnimplementedToolServiceHandler
	execute func(ctx context.Context, stream *connect.ServerStream[workerv1.ExecuteResponse]) error
}

func (s stub) Execute(ctx context.Context, _ *connect.Request[workerv1.ExecuteRequest], stream *connect.ServerStream[workerv1.ExecuteResponse]) error {
	return s.execute(ctx, stream)
}

func startStub(t *testing.T, execute func(ctx context.Context, stream *connect.ServerStream[workerv1.ExecuteResponse]) error) *Client {
	t.Helper()
	_, h := workerv1connect.NewToolServiceHandler(stub{execute: execute})
	c := NewClient(serve(t, h).URL, testToken, nil)
	t.Cleanup(c.CloseIdle)
	return c
}

func TestCanceledCodeWithLiveContextIsToolError(t *testing.T) {
	c := startStub(t, func(context.Context, *connect.ServerStream[workerv1.ExecuteResponse]) error {
		return connect.NewError(connect.CodeCanceled, errors.New("worker gave up"))
	})
	_, err := remote(c, "x").Execute(t.Context(), durable.ToolCall{ID: "1", Name: "x"})
	require.Error(t, err)
	require.NotErrorIs(t, err, context.Canceled)
	require.Equal(t, connect.CodeCanceled, connect.CodeOf(err))
	require.ErrorContains(t, err, "failed to execute x on worker")
}

func TestDeltasCappedFromAHostileWorker(t *testing.T) {
	chunk := strings.Repeat("x", 64<<10)
	c := startStub(t, func(_ context.Context, stream *connect.ServerStream[workerv1.ExecuteResponse]) error {
		for range 40 {
			if err := stream.Send(&workerv1.ExecuteResponse{Event: &workerv1.ExecuteResponse_Delta{Delta: chunk}}); err != nil {
				return err
			}
		}
		return stream.Send(&workerv1.ExecuteResponse{Event: &workerv1.ExecuteResponse_Result{Result: &workerv1.ExecuteResult{Output: "done"}}})
	})
	var got strings.Builder
	var markers int
	ctx := durable.WithToolOutputSink(t.Context(), func(s string) {
		if s == tools.LiveCapMarker {
			markers++
			return
		}
		got.WriteString(s)
	})
	out, err := remote(c, "x").Execute(ctx, durable.ToolCall{ID: "1", Name: "x"})
	require.NoError(t, err)
	require.Equal(t, "done", out)
	require.Equal(t, tools.MaxLiveOutputBytes, got.Len())
	require.Equal(t, 1, markers)
}

func TestOversizedMessageRejected(t *testing.T) {
	c := startStub(t, func(_ context.Context, stream *connect.ServerStream[workerv1.ExecuteResponse]) error {
		return stream.Send(&workerv1.ExecuteResponse{Event: &workerv1.ExecuteResponse_Delta{Delta: strings.Repeat("x", maxMessageBytes+1)}})
	})
	_, err := remote(c, "x").Execute(t.Context(), durable.ToolCall{ID: "1", Name: "x"})
	require.ErrorContains(t, err, "failed to execute x on worker")
}

func TestStreamWithoutResultIsError(t *testing.T) {
	c := startStub(t, func(context.Context, *connect.ServerStream[workerv1.ExecuteResponse]) error { return nil })
	_, err := remote(c, "x").Execute(t.Context(), durable.ToolCall{ID: "1", Name: "x"})
	require.ErrorContains(t, err, "without a result")
}
