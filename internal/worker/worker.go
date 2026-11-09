// Package worker serves a set of tools over Connect so a server can run them inside a sandbox.
package worker

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"connectrpc.com/connect"

	"github.com/wolfeidau/durable"
	workerv1 "github.com/wolfeidau/durable/api/durable/worker/v1"
	"github.com/wolfeidau/durable/api/durable/worker/v1/workerv1connect"
)

// TokenEnv is the environment variable holding the bearer token; Info never reports it.
const TokenEnv = "DURABLE_WORKER_TOKEN"

// deltaBuffer bounds the live output queued for a slow reader; the rest is dropped.
const deltaBuffer = 1024

// Config configures a Handler.
type Config struct {
	Tools []durable.Tool
	Root  string
	Token string
}

type call struct {
	done   chan struct{}
	cancel context.CancelFunc
	deltas chan string // nil unless the starting request asked for output
	// output and isErr are written before done closes and read only after.
	output string
	isErr  bool
}

type service struct {
	cfg   Config
	mu    sync.Mutex
	calls map[string]*call
}

// authInterceptor checks the bearer token on unary and streaming handlers.
type authInterceptor struct{ token string }

var _ connect.Interceptor = authInterceptor{}

func (a authInterceptor) check(h http.Header) error {
	got, _ := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) != 1 {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token"))
	}
	return nil
}

func (a authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := a.check(req.Header()); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (a authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := a.check(conn.RequestHeader()); err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// Handler returns the authenticated HTTP handler for the tool service.
func Handler(cfg Config) (http.Handler, error) {
	if cfg.Token == "" {
		return nil, errors.New("worker token must not be empty")
	}
	svc := &service{cfg: cfg, calls: map[string]*call{}}
	_, h := workerv1connect.NewToolServiceHandler(svc, connect.WithInterceptors(authInterceptor{token: cfg.Token}))
	return h, nil
}

func (s *service) Info(context.Context, *connect.Request[workerv1.InfoRequest]) (*connect.Response[workerv1.InfoResponse], error) {
	host, _ := os.Hostname()
	cwd, _ := os.Getwd()
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if k != TokenEnv {
			env[k] = v
		}
	}
	return connect.NewResponse(&workerv1.InfoResponse{
		Hostname: host, Uid: int64(os.Getuid()), Cwd: cwd, Root: s.cfg.Root, Env: env,
	}), nil
}

func (s *service) ListTools(context.Context, *connect.Request[workerv1.ListToolsRequest]) (*connect.Response[workerv1.ListToolsResponse], error) {
	out := make([]*workerv1.ToolInfo, 0, len(s.cfg.Tools))
	for _, t := range s.cfg.Tools {
		spec := t.Spec()
		out = append(out, &workerv1.ToolInfo{Name: spec.Name, Description: spec.Description, InputSchema: spec.Schema})
	}
	return connect.NewResponse(&workerv1.ListToolsResponse{Tools: out}), nil
}

func (s *service) tool(name string) durable.Tool {
	for _, t := range s.cfg.Tools {
		if t.Spec().Name == name {
			return t
		}
	}
	return nil
}

func (s *service) Execute(ctx context.Context, req *connect.Request[workerv1.ExecuteRequest], stream *connect.ServerStream[workerv1.ExecuteResponse]) error {
	m := req.Msg
	if m.CallId == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("call_id must not be empty"))
	}
	s.mu.Lock()
	c, known := s.calls[m.CallId]
	if !known {
		tool := s.tool(m.Name)
		if tool == nil {
			s.mu.Unlock()
			return connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown tool %q", m.Name))
		}
		c = s.start(ctx, tool, m)
		s.calls[m.CallId] = c
	}
	s.mu.Unlock()

	// Only the request that started the call streams; a known id gets just the result.
	var deltas chan string
	if !known {
		deltas = c.deltas
	}
	return streamCall(ctx, stream, c, deltas)
}

// start runs the call detached from the request so a dropped connection never stops it.
func (s *service) start(ctx context.Context, tool durable.Tool, m *workerv1.ExecuteRequest) *call {
	cctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c := &call{done: make(chan struct{}), cancel: cancel}
	if m.StreamOutput {
		c.deltas = make(chan string, deltaBuffer)
		cctx = durable.WithToolOutputSink(cctx, func(text string) {
			select {
			case c.deltas <- text:
			default: // live output is best-effort; a slow reader must not stall the tool
			}
		})
	}
	go func() {
		defer cancel()
		out, err := tool.Execute(cctx, durable.ToolCall{ID: m.CallId, Name: m.Name, Input: m.Input})
		if err != nil {
			out, c.isErr = err.Error(), true
		}
		c.output = out
		close(c.done)
	}()
	return c
}

func sendDelta(stream *connect.ServerStream[workerv1.ExecuteResponse], d string) error {
	return stream.Send(&workerv1.ExecuteResponse{Event: &workerv1.ExecuteResponse_Delta{Delta: d}})
}

// streamCall forwards deltas until the call is done, then sends the result.
func streamCall(ctx context.Context, stream *connect.ServerStream[workerv1.ExecuteResponse], c *call, deltas chan string) error {
	for {
		select {
		case d := <-deltas:
			if err := sendDelta(stream, d); err != nil {
				return err
			}
		case <-c.done:
			// The tool's sends happen before done closes, so draining the buffer is complete.
			for drained := deltas == nil; !drained; {
				select {
				case d := <-deltas:
					if err := sendDelta(stream, d); err != nil {
						return err
					}
				default:
					drained = true
				}
			}
			return stream.Send(&workerv1.ExecuteResponse{Event: &workerv1.ExecuteResponse_Result{
				Result: &workerv1.ExecuteResult{Output: c.output, IsError: c.isErr},
			}})
		case <-ctx.Done():
			return connect.NewError(connect.CodeCanceled, ctx.Err())
		}
	}
}

func (s *service) lookup(id string) *call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[id]
}

func (s *service) Cancel(_ context.Context, req *connect.Request[workerv1.CancelRequest]) (*connect.Response[workerv1.CancelResponse], error) {
	if c := s.lookup(req.Msg.CallId); c != nil {
		c.cancel()
	}
	return connect.NewResponse(&workerv1.CancelResponse{}), nil
}

func (s *service) Result(_ context.Context, req *connect.Request[workerv1.ResultRequest]) (*connect.Response[workerv1.ResultResponse], error) {
	c := s.lookup(req.Msg.CallId)
	if c == nil {
		return connect.NewResponse(&workerv1.ResultResponse{}), nil
	}
	select {
	case <-c.done:
		return connect.NewResponse(&workerv1.ResultResponse{Found: true, Output: c.output, IsError: c.isErr}), nil
	default:
		return connect.NewResponse(&workerv1.ResultResponse{}), nil
	}
}
