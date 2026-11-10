package openshell

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"
	"github.com/ai-openshell/sdk-go/openshell/v1/fake"
	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/worker"
	"github.com/wolfeidau/durable/tools"
)

const testToken = "test-token"

// trackedConn counts closes so tests can see that a forward was dropped.
type trackedConn struct {
	net.Conn
	closed *atomic.Int32
	once   sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() { c.closed.Add(1) })
	return c.Conn.Close()
}

// forwarder stands in for the gateway: each Forward dials addr, which tests can change or leave unreachable.
type forwarder struct {
	addr    atomic.Value
	opened  atomic.Int32
	closed  atomic.Int32
	lastReq atomic.Value
}

func (f *forwarder) Forward(ctx context.Context, workspace, name string, port uint32, _ ...v1.ForwardOption) (io.ReadWriteCloser, error) {
	f.lastReq.Store([3]any{workspace, name, port})
	conn, err := new(net.Dialer).DialContext(ctx, "tcp", f.addr.Load().(string))
	if err != nil {
		return nil, err
	}
	f.opened.Add(1)
	return &trackedConn{Conn: conn, closed: &f.closed}, nil
}

func (f *forwarder) Listen(context.Context, string, string, uint32, uint32, ...v1.ListenOption) (v1.ForwardListener, error) {
	return nil, io.ErrUnexpectedEOF
}

type forwardingClient struct {
	*fake.Client
	tcp v1.TCPInterface
}

func (c forwardingClient) TCP() v1.TCPInterface { return c.tcp }

func startWorker(t *testing.T) string {
	t.Helper()
	all, err := tools.All(t.TempDir(), t.TempDir())
	require.NoError(t, err)
	h, err := worker.Handler(worker.Config{Tools: all, Root: t.TempDir(), Token: testToken})
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(h)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// unreachableAddr is a local address nothing listens on.
func unreachableAddr(t *testing.T) string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func newForwarded(t *testing.T, addr string) (*Launcher, *forwarder) {
	t.Helper()
	f := &forwarder{}
	f.addr.Store(addr)
	return newWithClient(forwardingClient{Client: fake.NewClient(), tcp: f}, ""), f
}

func TestConnectReachesWorker(t *testing.T) {
	l, f := newForwarded(t, startWorker(t))

	conn, err := l.Connect(t.Context(), "dt-a", testToken)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	got, err := conn.Client().Tools(t.Context())
	require.NoError(t, err)
	var names []string
	for _, tl := range got {
		names = append(names, tl.Spec().Name)
	}
	require.Len(t, names, len(tools.Catalog()))
	require.Contains(t, names, "bash")
	require.Equal(t, [3]any{"default", "dt-a", uint32(WorkerPort)}, f.lastReq.Load())
}

func TestRequestAfterDroppedForwardRedials(t *testing.T) {
	l, f := newForwarded(t, startWorker(t))
	conn, err := l.Connect(t.Context(), "dt-a", testToken)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	before := f.opened.Load()

	conn.conns.closeAll()

	require.Eventually(t, func() bool {
		_, err := conn.Client().Tools(t.Context())
		return err == nil && f.opened.Load() > before
	}, 5*time.Second, 50*time.Millisecond)
}

func TestConnCloseClosesForwards(t *testing.T) {
	l, f := newForwarded(t, startWorker(t))
	conn, err := l.Connect(t.Context(), "dt-a", testToken)
	require.NoError(t, err)
	require.Positive(t, f.opened.Load())

	require.NoError(t, conn.Close())

	require.Eventually(t, func() bool { return f.closed.Load() == f.opened.Load() }, 5*time.Second, 20*time.Millisecond)
}

func TestConnectPollsUntilWorkerListens(t *testing.T) {
	l, f := newForwarded(t, unreachableAddr(t))
	up := startWorker(t)
	time.AfterFunc(900*time.Millisecond, func() { f.addr.Store(up) })

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	conn, err := l.Connect(ctx, "dt-a", testToken)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.Positive(t, f.opened.Load())
}

func TestConnectFailsWhenContextEnds(t *testing.T) {
	l, _ := newForwarded(t, unreachableAddr(t))

	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	_, err := l.Connect(ctx, "dt-a", testToken)
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.True(t, IsUnavailable(err))
}

func TestConnectStopsOnWrongToken(t *testing.T) {
	l, _ := newForwarded(t, startWorker(t))

	start := time.Now()
	_, err := l.Connect(t.Context(), "dt-a", "wrong")
	require.Error(t, err)
	require.False(t, IsUnavailable(err))
	require.Less(t, time.Since(start), attemptTimeout)
}

func TestConnectStopsOnMismatchedCatalog(t *testing.T) {
	all, err := tools.All(t.TempDir(), t.TempDir())
	require.NoError(t, err)
	h, err := worker.Handler(worker.Config{Tools: all[:len(all)-1], Root: t.TempDir(), Token: testToken})
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(h)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	l, _ := newForwarded(t, srv.Listener.Addr().String())

	start := time.Now()
	_, err = l.Connect(t.Context(), "dt-a", testToken)
	require.ErrorContains(t, err, "worker offers")
	require.False(t, IsUnavailable(err))
	require.Less(t, time.Since(start), attemptTimeout)
}

func TestSDKStaysOutOfOtherBinaries(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", ".", "./cmd/durable", "./cmd/durable-worker")
	cmd.Dir = "../../.."
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	for pkg := range strings.FieldsSeq(string(out)) {
		require.NotContains(t, pkg, "github.com/ai-openshell", "the SDK must not be linked outside the server")
	}
}
