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
