package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wolfeidau/durable/internal/worker"
)

const (
	handshakePrefix = "durable-worker listening "
	closeTimeout    = 10 * time.Second
	stderrCap       = 4096
)

// Launcher starts a worker.
type Launcher interface {
	Start(ctx context.Context) (*Worker, error)
}

// Worker is a started worker and the client that reaches it.
type Worker struct {
	client *Client
	cmd    *exec.Cmd
	waited chan struct{}
	once   sync.Once
}

// Client returns the client for the worker.
func (w *Worker) Client() *Client { return w.client }

// Close stops the worker with SIGTERM and kills it if it does not exit in time.
func (w *Worker) Close() error {
	w.once.Do(func() {
		w.client.CloseIdle()
		if w.cmd == nil {
			return
		}
		_ = w.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-w.waited:
		case <-time.After(closeTimeout):
			_ = w.cmd.Process.Kill()
			<-w.waited
		}
	})
	return nil
}

// LocalLauncher runs the worker binary as a child process on this machine.
type LocalLauncher struct {
	Binary   string
	Root     string
	StateDir string
}

var _ Launcher = LocalLauncher{}

// Start launches the worker and waits for its handshake line, bounded by ctx.
func (l LocalLauncher) Start(ctx context.Context) (*Worker, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("failed to generate worker token: %w", err)
	}
	token := hex.EncodeToString(raw)

	// WithoutCancel because ctx bounds only the handshake, not the worker's lifetime.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), l.Binary, "--addr", "127.0.0.1:0", "--root", l.Root, "--state-dir", l.StateDir) //nolint:gosec // the binary is operator configuration
	// The token travels in the child's environment only, because argv is visible to other processes.
	cmd.Env = append(withoutEnv(os.Environ(), worker.TokenEnv), worker.TokenEnv+"="+token)
	stderr := &capBuffer{}
	cmd.Stderr = stderr
	// An os.Pipe rather than StdoutPipe, because Wait closes StdoutPipe and could drop an unread handshake.
	stdout, pw, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("failed to pipe worker stdout: %w", err)
	}
	cmd.Stdout = pw
	err = cmd.Start()
	_ = pw.Close()
	if err != nil {
		_ = stdout.Close()
		return nil, fmt.Errorf("failed to start worker: %w", err)
	}
	w := &Worker{cmd: cmd, waited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(w.waited)
	}()

	type handshake struct {
		addr string
		err  error
	}
	hs := make(chan handshake, 1)
	go func() {
		defer stdout.Close()
		line, err := bufio.NewReader(stdout).ReadString('\n')
		addr, ok := strings.CutPrefix(strings.TrimSpace(line), handshakePrefix)
		switch {
		case err != nil:
			hs <- handshake{err: fmt.Errorf("failed to read worker handshake: %w", err)}
		case !ok || addr == "":
			hs <- handshake{err: errors.New("worker printed an unexpected handshake line")}
		default:
			hs <- handshake{addr: addr}
		}
	}()

	select {
	case h := <-hs:
		if h.err != nil {
			w.stop()
			return nil, fmt.Errorf("%w; worker stderr: %s", h.err, stderr.String())
		}
		w.client = NewClient("http://"+h.addr, token, nil)
		return w, nil
	case <-ctx.Done():
		w.stop()
		return nil, fmt.Errorf("failed to start worker: %w; worker stderr: %s", ctx.Err(), stderr.String())
	}
}

// stop kills a worker that never completed its handshake.
func (w *Worker) stop() {
	_ = w.cmd.Process.Kill()
	<-w.waited
}

func withoutEnv(env []string, name string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != name {
			out = append(out, kv)
		}
	}
	return out
}

// capBuffer keeps the first stderrCap bytes, enough to explain a failed start.
type capBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := stderrCap - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *capBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(b.buf.String())
}
