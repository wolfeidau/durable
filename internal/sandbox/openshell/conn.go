package openshell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/wolfeidau/durable/internal/sandbox"
)

const (
	attemptTimeout = 2 * time.Second
	attemptBackoff = 300 * time.Millisecond
)

var errWorkerUnreachable = errors.New("worker did not answer")

// Conn is a client for one sandbox's worker, over forward streams through the gateway.
type Conn struct {
	client *sandbox.Client
	conns  *connSet
}

// Client returns the client for the worker.
func (c *Conn) Client() *sandbox.Client { return c.client }

// Close closes every open forward.
func (c *Conn) Close() error {
	c.client.CloseIdle()
	c.conns.closeAll()
	return nil
}

// Connect dials the worker and polls until it answers or ctx ends, because the worker may not be listening yet
// when the sandbox first reports Ready.
func (l *Launcher) Connect(ctx context.Context, name, token string) (*Conn, error) {
	conns := &connSet{conns: map[*forwardConn]struct{}{}}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		// The stream must outlive the dial's context, which ends with the request that triggered it.
		sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		rwc, err := l.c.TCP().Forward(sctx, l.ws, name, WorkerPort)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("failed to forward to sandbox: %w", err)
		}
		fc := &forwardConn{rwc: rwc, cancel: cancel, set: conns}
		conns.add(fc)
		return fc, nil
	}
	conn := &Conn{client: sandbox.NewDialClient("http://worker.sandbox", token, dial), conns: conns}

	for {
		actx, cancel := context.WithTimeout(ctx, attemptTimeout)
		_, err := conn.client.Tools(actx)
		cancel()
		if err == nil {
			return conn, nil
		}
		// Only a worker that isn't listening yet is worth waiting for; a wrong token or catalog won't fix itself.
		if code := connect.CodeOf(err); code != connect.CodeUnavailable && code != connect.CodeDeadlineExceeded {
			_ = conn.Close()
			return nil, fmt.Errorf("failed to connect to worker: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = conn.Close()
			// Only a timeout means the worker is unreachable; a cancel is the caller giving up.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("%w: %w; last error: %w", errWorkerUnreachable, ctx.Err(), err)
			}
			return nil, fmt.Errorf("failed to connect to worker: %w; last error: %w", ctx.Err(), err)
		case <-time.After(attemptBackoff):
		}
	}
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
	all := make([]*forwardConn, 0, len(s.conns))
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
