package remote

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"
	"github.com/ai-openshell/sdk-go/openshell/v1/gateway"
	"github.com/ai-openshell/sdk-go/openshell/v1/types"
)

// Workspace is the OpenShell workspace sandboxes are created in.
const Workspace = "default"

// WorkerPort is the port the worker listens on inside the sandbox.
const WorkerPort = 8080

// SDKLauncher drives OpenShell through its Go SDK instead of the CLI.
type SDKLauncher struct {
	c *v1.Client
}

// NewSDKLauncher connects to the active local gateway.
func NewSDKLauncher(_ context.Context) (*SDKLauncher, error) {
	cfg, err := gateway.LoadConfig("")
	if err != nil {
		return nil, fmt.Errorf("failed to load gateway config: %w", err)
	}
	// The SDK has no mTLS auth mode; the client certificate goes through the TLS config instead.
	c, err := gateway.NewClient(cfg.Name,
		gateway.WithAuth(v1.NoAuth()),
		gateway.WithTLS(&types.TLSConfig{
			CertFile: filepath.Join(cfg.Dir, "mtls", "tls.crt"),
			KeyFile:  filepath.Join(cfg.Dir, "mtls", "tls.key"),
			CAFile:   filepath.Join(cfg.Dir, "mtls", "ca.crt"),
		}))
	if err != nil {
		return nil, fmt.Errorf("failed to create gateway client: %w", err)
	}
	return &SDKLauncher{c: c}, nil
}

// Close releases the gateway connection.
func (l *SDKLauncher) Close() error { return l.c.Close() }

// Workspaces lists workspace names, to confirm Workspace exists.
func (l *SDKLauncher) Workspaces(ctx context.Context) ([]string, error) {
	ws, err := l.c.Workspaces().ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list workspaces: %w", err)
	}
	var names []string
	for _, w := range ws {
		names = append(names, w.Name)
	}
	return names, nil
}

// Sandboxes lists sandbox names.
func (l *SDKLauncher) Sandboxes(ctx context.Context) ([]string, error) {
	sbs, err := l.c.Sandboxes().ListAll(ctx, Workspace)
	if err != nil {
		return nil, fmt.Errorf("failed to list sandboxes: %w", err)
	}
	var names []string
	for _, s := range sbs {
		names = append(names, s.Name)
	}
	return names, nil
}

// Create starts a sandbox running the worker and waits until the sandbox is ready. The token is passed in the
// sandbox environment only.
func (l *SDKLauncher) Create(ctx context.Context, name, image, token string, workerArgs []string) error {
	cmd := append([]string{WorkerBinary, "--addr", "127.0.0.1:" + strconv.Itoa(WorkerPort), "--root", SandboxRoot}, workerArgs...)
	spec := &v1.SandboxSpec{
		Environment: map[string]string{"DURABLE_WORKER_TOKEN": token},
		Template:    &v1.SandboxTemplate{Image: image},
		Command:     cmd,
	}
	if _, err := l.c.Sandboxes().Create(ctx, Workspace, name, spec); err != nil {
		return fmt.Errorf("failed to create sandbox: %w", err)
	}
	if _, err := l.c.Sandboxes().WaitReady(ctx, Workspace, name); err != nil {
		return fmt.Errorf("failed to wait for sandbox: %w", err)
	}
	return nil
}

// Dial returns a client that reaches the worker's port through the gateway. Each connection is a new forward
// stream, so a lost connection is replaced on the next call without any other action.
func (l *SDKLauncher) Dial(name, token string, port int) *Client {
	conns := &connSet{conns: map[*forwardConn]struct{}{}}
	transport := h2cTransport()
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		// The stream must outlive the dial's context, which ends with the request that triggered it.
		sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		rwc, err := l.c.TCP().Forward(sctx, Workspace, name, uint32(port)) //nolint:gosec // port is a small constant
		if err != nil {
			cancel()
			return nil, fmt.Errorf("failed to forward to sandbox: %w", err)
		}
		fc := &forwardConn{rwc: rwc, cancel: cancel, set: conns}
		conns.add(fc)
		return fc, nil
	}
	c := newClient(&http.Client{Transport: transport}, "http://worker.sandbox", token)
	c.drop = conns.closeAll
	return c
}

// Stop stops the sandbox and waits until it has stopped.
func (l *SDKLauncher) Stop(ctx context.Context, name string) error {
	if _, err := l.c.Sandboxes().Stop(ctx, Workspace, name); err != nil {
		return fmt.Errorf("failed to stop sandbox: %w", err)
	}
	if _, err := l.c.Sandboxes().WaitStopped(ctx, Workspace, name); err != nil {
		return fmt.Errorf("failed to wait for sandbox to stop: %w", err)
	}
	return nil
}

// Start starts a stopped sandbox and waits until it is ready.
func (l *SDKLauncher) Start(ctx context.Context, name string) error {
	if _, err := l.c.Sandboxes().Start(ctx, Workspace, name); err != nil {
		return fmt.Errorf("failed to start sandbox: %w", err)
	}
	if _, err := l.c.Sandboxes().WaitReady(ctx, Workspace, name); err != nil {
		return fmt.Errorf("failed to wait for sandbox: %w", err)
	}
	return nil
}

// Delete removes the sandbox; a missing sandbox is not an error.
func (l *SDKLauncher) Delete(ctx context.Context, name string) error {
	if _, err := l.c.Sandboxes().Delete(ctx, Workspace, name, v1.DeleteOptions{AllowMissing: true}); err != nil {
		return fmt.Errorf("failed to delete sandbox: %w", err)
	}
	return nil
}

// Logs returns the sandbox's log lines.
func (l *SDKLauncher) Logs(ctx context.Context, name string) ([]string, error) {
	res, err := l.c.Sandboxes().GetLogs(ctx, Workspace, name)
	if err != nil {
		return nil, fmt.Errorf("failed to get sandbox logs: %w", err)
	}
	var lines []string
	for _, ln := range res.Lines {
		lines = append(lines, fmt.Sprintf("%s [%s] %s", ln.Source, ln.Level, ln.Message))
	}
	return lines, nil
}

// Upload copies the contents of localDir into dest in the sandbox by streaming a tar archive to `tar -x`,
// because the SDK's file transfer is unavailable. Regular files and directories only.
func (l *SDKLauncher) Upload(ctx context.Context, name, localDir, dest string) error {
	// Interactive allocates a PTY: raw mode keeps the bytes intact, tar refuses a terminal, and the PTY
	// never signals end of input, so head reads exactly the archive's length.
	var archive bytes.Buffer
	if err := writeTar(&archive, localDir); err != nil {
		return err
	}
	argv := []string{"sh", "-c", `stty raw -echo && echo READY && mkdir -p "$1" && head -c "$2" | tar -xf - -C "$1"`, "sh", dest, strconv.Itoa(archive.Len())}
	sess, err := l.c.Exec().Interactive(ctx, Workspace, name, argv, 0, 0)
	if err != nil {
		return fmt.Errorf("failed to start tar in sandbox: %w", err)
	}
	defer sess.Close()
	var out strings.Builder
	drained := make(chan struct{})
	ready := make(chan struct{})
	go func() {
		defer close(drained)
		buf := make([]byte, 4096)
		signalled := false
		for {
			n, err := sess.Read(buf)
			if out.Len() < 64<<10 {
				out.Write(buf[:n])
			}
			if !signalled && strings.Contains(out.String(), "READY") {
				signalled = true
				close(ready)
			}
			if err != nil {
				return
			}
		}
	}()
	// Data sent before stty runs would be mangled by the terminal's line discipline.
	select {
	case <-ready:
	case <-drained:
	case <-time.After(15 * time.Second):
	}
	// The gateway rejects stdin messages over 1 MiB, and each Write is one message.
	_, werr := io.CopyBuffer(sess, io.LimitReader(&archive, int64(archive.Len())), make([]byte, 256<<10))
	if werr != nil {
		werr = fmt.Errorf("failed to send tar: %w", werr)
	}
	cerr := v1.CloseInteractiveInput(sess)
	code, xerr := sess.ExitCode()
	<-drained
	if werr != nil {
		return werr
	}
	if cerr != nil {
		return fmt.Errorf("failed to close tar input: %w", cerr)
	}
	if xerr != nil {
		return fmt.Errorf("failed to wait for tar: %w", xerr)
	}
	if code != 0 {
		msg := strings.ToValidUTF8(out.String(), "?")
		return fmt.Errorf("tar exited %d: %s", code, strings.TrimSpace(msg[max(0, len(msg)-300):]))
	}
	return nil
}

func writeTar(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path) //nolint:gosec // walking the caller's directory
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to write tar of %s: %w", dir, err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("failed to finish tar: %w", err)
	}
	return nil
}

type connSet struct {
	mu    sync.Mutex
	conns map[*forwardConn]struct{}
}

func (s *connSet) add(c *forwardConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conns[c] = struct{}{}
}

func (s *connSet) remove(c *forwardConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
}

func (s *connSet) closeAll() {
	s.mu.Lock()
	var all []*forwardConn
	for c := range s.conns {
		all = append(all, c)
	}
	s.mu.Unlock()
	for _, c := range all {
		_ = c.Close()
	}
}

// forwardConn adapts a forward stream to net.Conn. Deadlines are not supported.
type forwardConn struct {
	rwc    io.ReadWriteCloser
	cancel context.CancelFunc
	set    *connSet
	once   sync.Once
}

var _ net.Conn = (*forwardConn)(nil)

type streamAddr struct{}

func (streamAddr) Network() string { return "openshell" }
func (streamAddr) String() string  { return "openshell-forward" }

func (c *forwardConn) Read(p []byte) (int, error)  { return c.rwc.Read(p) }
func (c *forwardConn) Write(p []byte) (int, error) { return c.rwc.Write(p) }
func (c *forwardConn) Close() error {
	var err error
	c.once.Do(func() {
		c.set.remove(c)
		err = c.rwc.Close()
		c.cancel()
	})
	return err
}
func (c *forwardConn) LocalAddr() net.Addr              { return streamAddr{} }
func (c *forwardConn) RemoteAddr() net.Addr             { return streamAddr{} }
func (c *forwardConn) SetDeadline(time.Time) error      { return nil }
func (c *forwardConn) SetReadDeadline(time.Time) error  { return nil }
func (c *forwardConn) SetWriteDeadline(time.Time) error { return nil }
