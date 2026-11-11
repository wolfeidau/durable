package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"connectrpc.com/connect"

	workerv1 "github.com/wolfeidau/durable/api/durable/worker/v1"
)

// WorkerBinary is where the worker image installs the worker.
const WorkerBinary = "/usr/local/bin/durable-worker"

const (
	// stdioChunk keeps one frame far under the worker's message cap.
	stdioChunk = 64 << 10
	// closeWait covers the worker's stdin-close, SIGTERM and SIGKILL steps before the stream is cut.
	closeWait = 6 * time.Second
	// runStderrCap bounds the stderr Run keeps.
	runStderrCap = 8 << 10
)

// StdioSpec is a process to run beside the worker.
type StdioSpec struct {
	Command string
	Args    []string
	Env     map[string]string
	// WorkspaceCwd runs the process in the workspace root; otherwise in a temporary directory.
	WorkspaceCwd bool
}

// Stdio runs spec on the worker and returns the process's stdin and stdout. Its stderr goes to stderr, which may be
// nil. The process lives until Close, not until ctx ends. Reads end with io.EOF when the process exits.
func (c *Client) Stdio(ctx context.Context, spec StdioSpec, stderr io.Writer) (io.ReadWriteCloser, error) {
	// Detached: the process must outlive the call that started it.
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stream := c.rpc.Stdio(sctx)
	start := &workerv1.StdioRequest{Event: &workerv1.StdioRequest_Start{Start: &workerv1.StdioStart{
		Command: spec.Command, Args: spec.Args, Env: spec.Env, WorkspaceCwd: spec.WorkspaceCwd,
	}}}
	if err := stream.Send(start); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to start %q on the worker: %w", spec.Command, err)
	}
	pr, pw := io.Pipe()
	s := &stdioConn{stream: stream, cancel: cancel, pr: pr, done: make(chan struct{})}
	go s.receive(pw, stderr)
	return s, nil
}

// Run runs spec on the worker with no stdin until it exits, and returns its exit code and the end of its stderr. The
// last bytes are kept, not the first, because a command's error comes last. If ctx ends first the stream is cut and the
// worker kills the process group.
func (c *Client) Run(ctx context.Context, spec StdioSpec) (exitCode int, stderr []byte, err error) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := c.rpc.Stdio(sctx)
	defer func() { _ = stream.CloseResponse() }()
	start := &workerv1.StdioRequest{Event: &workerv1.StdioRequest_Start{Start: &workerv1.StdioStart{
		Command: spec.Command, Args: spec.Args, Env: spec.Env, WorkspaceCwd: spec.WorkspaceCwd,
	}}}
	if err := stream.Send(start); err != nil {
		return 0, nil, fmt.Errorf("failed to start %q on the worker: %w", spec.Command, err)
	}
	// The request side stays open: the worker treats its end as the end of the process.
	for {
		msg, err := stream.Receive()
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				err = errors.New("worker stream ended without an exit code")
			}
			return 0, nil, fmt.Errorf("failed to run %q on the worker: %w", spec.Command, err)
		}
		switch ev := msg.Event.(type) {
		case *workerv1.StdioResponse_Stderr:
			stderr = append(stderr, ev.Stderr...)
			if len(stderr) > runStderrCap {
				stderr = append([]byte(nil), stderr[len(stderr)-runStderrCap:]...)
			}
		case *workerv1.StdioResponse_ExitCode:
			return int(ev.ExitCode), stderr, nil
		}
	}
}

type stdioConn struct {
	stream *connect.BidiStreamForClient[workerv1.StdioRequest, workerv1.StdioResponse]
	cancel context.CancelFunc
	pr     *io.PipeReader
	done   chan struct{}

	wmu       sync.Mutex
	closeOnce sync.Once
}

func (s *stdioConn) receive(pw *io.PipeWriter, stderr io.Writer) {
	defer close(s.done)
	for {
		msg, err := s.stream.Receive()
		if err != nil {
			// A clean end without an exit code is still the worker going away.
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			pw.CloseWithError(err)
			return
		}
		switch ev := msg.Event.(type) {
		case *workerv1.StdioResponse_Stdout:
			if _, err := pw.Write(ev.Stdout); err != nil {
				return
			}
		case *workerv1.StdioResponse_Stderr:
			if stderr != nil {
				_, _ = stderr.Write(ev.Stderr)
			}
		case *workerv1.StdioResponse_ExitCode:
			_ = pw.Close()
			return
		}
	}
}

func (s *stdioConn) Read(p []byte) (int, error) { return s.pr.Read(p) }

func (s *stdioConn) Write(p []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	written := 0
	for len(p) > 0 {
		n := min(len(p), stdioChunk)
		if err := s.stream.Send(&workerv1.StdioRequest{Event: &workerv1.StdioRequest_Stdin{Stdin: append([]byte(nil), p[:n]...)}}); err != nil {
			return written, fmt.Errorf("failed to write to the worker process: %w", err)
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// Close ends stdin, which stops the process, and waits for the worker's exit frame before cutting the stream.
func (s *stdioConn) Close() error {
	s.closeOnce.Do(func() {
		s.wmu.Lock()
		_ = s.stream.CloseRequest()
		s.wmu.Unlock()
		select {
		case <-s.done:
		case <-time.After(closeWait):
		}
		s.cancel()
		_ = s.stream.CloseResponse()
		_ = s.pr.Close()
	})
	return nil
}
