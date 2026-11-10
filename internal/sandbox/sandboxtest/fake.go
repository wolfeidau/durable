// Package sandboxtest is an in-memory sandbox.Backend for tests. Connect dials a real worker, so remote tools run.
package sandboxtest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/worker"
	"github.com/wolfeidau/durable/tools"
)

// ErrUnavailable is the failure IsUnavailable reports as retryable.
var ErrUnavailable = errors.New("backend unavailable")

// Phases a Fake reports.
const (
	Ready   = "ready"
	Stopped = "stopped"
)

// Fake is a sandbox.Backend that keeps phases in memory.
type Fake struct {
	t *testing.T

	mu         sync.Mutex
	phase      map[string]string
	specs      []sandbox.Spec
	ensures    int
	stops      int
	deletes    int
	ensureErr  error
	connectErr error
}

var _ sandbox.Backend = (*Fake)(nil)

// New returns a Fake whose workers are cleaned up with t.
func New(t *testing.T) *Fake {
	t.Helper()
	return &Fake{t: t, phase: map[string]string{}}
}

// FailEnsure makes Ensure return err until it is called with nil.
func (f *Fake) FailEnsure(err error) { f.mu.Lock(); f.ensureErr = err; f.mu.Unlock() }

// FailConnect makes Connect return err until it is called with nil.
func (f *Fake) FailConnect(err error) { f.mu.Lock(); f.connectErr = err; f.mu.Unlock() }

// Counts returns how many times Ensure, Stop and Delete were called.
func (f *Fake) Counts() (ensures, stops, deletes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ensures, f.stops, f.deletes
}

// Specs returns the specs Ensure was called with, in order.
func (f *Fake) Specs() []sandbox.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sandbox.Spec(nil), f.specs...)
}

// Phase returns the sandbox's phase, empty when it doesn't exist.
func (f *Fake) Phase(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.phase[name]
}

func (f *Fake) Ensure(_ context.Context, s sandbox.Spec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensures++
	f.specs = append(f.specs, s)
	if f.ensureErr != nil {
		return f.ensureErr
	}
	f.phase[s.Name] = Ready
	return nil
}

func (f *Fake) Stop(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	if _, ok := f.phase[name]; ok {
		f.phase[name] = Stopped
	}
	return nil
}

func (f *Fake) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	delete(f.phase, name)
	return nil
}

func (f *Fake) Connect(_ context.Context, _, token string) (*sandbox.Client, io.Closer, error) {
	f.mu.Lock()
	err := f.connectErr
	f.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	all, err := tools.All(f.t.TempDir(), f.t.TempDir())
	require.NoError(f.t, err)
	h, err := worker.Handler(worker.Config{Tools: all, Root: f.t.TempDir(), Token: token})
	require.NoError(f.t, err)
	srv := httptest.NewUnstartedServer(h)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	f.t.Cleanup(srv.Close)
	c := sandbox.NewClient("http://"+srv.Listener.Addr().String(), token, nil)
	return c, closeFunc(c.CloseIdle), nil
}

func (*Fake) IsUnavailable(err error) bool { return errors.Is(err, ErrUnavailable) }

type closeFunc func()

func (c closeFunc) Close() error { c(); return nil }
