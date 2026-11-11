package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"

	workerv1 "github.com/wolfeidau/durable/api/durable/worker/v1"
)

const (
	// stdioChunk keeps one frame far under the client's message cap.
	stdioChunk = 64 << 10
	// stdioGrace matches the SDK's CommandTransport: stdin closes, then SIGTERM, then SIGKILL, each after this long.
	stdioGrace = 2 * time.Second
)

// Stdio runs the requested process in its own process group and pipes it over the stream. It ends the whole group when
// the stream ends, so nothing the server started outlives its session.
func (s *service) Stdio(ctx context.Context, stream *connect.BidiStream[workerv1.StdioRequest, workerv1.StdioResponse]) error {
	first, err := stream.Receive()
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("failed to read start: %w", err))
	}
	start := first.GetStart()
	if start == nil || start.Command == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("first request must start a command"))
	}

	proc, err := startStdio(start, s.cfg.Root)
	if err != nil {
		return err
	}
	defer proc.close()
	stdin, outR, errR := proc.stdin, proc.outR, proc.errR
	pgid := proc.cmd.Process.Pid
	signalGroup := func(sig syscall.Signal) { _ = syscall.Kill(-pgid, sig) }
	cmd := proc.cmd

	exited := make(chan struct{})
	var code int
	go func() {
		_ = cmd.Wait()
		// Whatever the server left behind goes with it.
		signalGroup(syscall.SIGKILL)
		code = cmd.ProcessState.ExitCode()
		close(exited)
	}()

	// The request side ending, or the client going away, is the end of the stream.
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		skip := false
		for {
			req, err := stream.Receive()
			if err != nil {
				return
			}
			if in := req.GetStdin(); in != nil && !skip {
				_, werr := stdin.Write(in)
				skip = werr != nil
			}
		}
	}()
	go func() {
		select {
		case <-ended:
		case <-ctx.Done():
		case <-exited:
			return
		}
		// Closing also frees a write blocked on a process that does not read.
		_ = stdin.Close()
		terminate(exited, signalGroup)
	}()

	var sendMu sync.Mutex
	send := func(r *workerv1.StdioResponse) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(r)
	}
	var wg sync.WaitGroup
	pump := func(f *os.File, wrap func([]byte) *workerv1.StdioResponse) {
		defer wg.Done()
		buf := make([]byte, stdioChunk)
		var sendErr error
		for {
			n, err := f.Read(buf)
			// After a failed send the stream is gone; keep draining so the process does not block on a full pipe.
			if n > 0 && sendErr == nil {
				sendErr = send(wrap(append([]byte(nil), buf[:n]...)))
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go pump(outR, func(b []byte) *workerv1.StdioResponse {
		return &workerv1.StdioResponse{Event: &workerv1.StdioResponse_Stdout{Stdout: b}}
	})
	go pump(errR, func(b []byte) *workerv1.StdioResponse {
		return &workerv1.StdioResponse{Event: &workerv1.StdioResponse_Stderr{Stderr: b}}
	})

	<-exited
	// A process that left its own session holds the pipes; give the rest of the output a moment, then stop reading.
	deadline := time.Now().Add(stdioGrace)
	_ = outR.SetReadDeadline(deadline)
	_ = errR.SetReadDeadline(deadline)
	wg.Wait()
	_ = send(&workerv1.StdioResponse{Event: &workerv1.StdioResponse_ExitCode{ExitCode: int32(code)}}) //nolint:gosec // exit codes are small
	return nil
}

// terminate stops the process group in steps unless it exits first.
func terminate(exited <-chan struct{}, signalGroup func(syscall.Signal)) {
	for _, sig := range []syscall.Signal{0, syscall.SIGTERM, syscall.SIGKILL} {
		if sig != 0 {
			signalGroup(sig)
		}
		select {
		case <-exited:
			return
		case <-time.After(stdioGrace):
		}
	}
}

type stdioProc struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	outR, errR *os.File
	removeDir  func()
}

func (p *stdioProc) close() {
	_ = p.outR.Close()
	_ = p.errR.Close()
	p.removeDir()
}

// startStdio starts the process in its own group, in the workspace or a temp folder.
func startStdio(start *workerv1.StdioStart, root string) (*stdioProc, error) {
	dir := root
	removeDir := func() {}
	if !start.WorkspaceCwd {
		tmp, err := os.MkdirTemp("", "durable-stdio-*")
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to create working directory: %w", err))
		}
		dir = tmp
		removeDir = func() { _ = os.RemoveAll(tmp) }
	}

	cmd := exec.Command(start.Command, start.Args...) //nolint:gosec,noctx // running the requested command is the point; the stream context must not signal it directly
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, TokenEnv+"=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "PWD="+dir)
	for k, v := range start.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	fail := func(code connect.Code, err error) (*stdioProc, error) {
		removeDir()
		return nil, connect.NewError(code, err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fail(connect.CodeInternal, fmt.Errorf("failed to open stdin: %w", err))
	}
	// Plain files, so Wait does not block on a grandchild that holds the pipe open.
	outR, outW, err := os.Pipe()
	if err != nil {
		return fail(connect.CodeInternal, fmt.Errorf("failed to open stdout: %w", err))
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return fail(connect.CodeInternal, fmt.Errorf("failed to open stderr: %w", err))
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	err = cmd.Start()
	_ = outW.Close()
	_ = errW.Close()
	if err != nil {
		_ = outR.Close()
		_ = errR.Close()
		return fail(connect.CodeInvalidArgument, fmt.Errorf("failed to start %q: %w", start.Command, err))
	}
	return &stdioProc{cmd: cmd, stdin: stdin, outR: outR, errR: errR, removeDir: removeDir}, nil
}
