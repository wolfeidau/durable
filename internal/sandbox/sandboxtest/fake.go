// Package sandboxtest is an in-memory sandbox.Backend for tests. Connect dials a real worker, so remote tools run.
package sandboxtest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strings"
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

	mu          sync.Mutex
	phase       map[string]string
	roots       map[string]string
	specs       []sandbox.Spec
	ensures     int
	stops       int
	deletes     int
	phasesCalls int
	ensureErr   error
	connectErr  error
	phasesErr   error
	stdios      int
	missing     []string
}

var _ sandbox.Backend = (*Fake)(nil)

// New returns a Fake whose workers are cleaned up with t.
func New(t *testing.T) *Fake {
	t.Helper()
	return &Fake{t: t, phase: map[string]string{}, roots: map[string]string{}}
}

// FailEnsure makes Ensure return err until it is called with nil.
func (f *Fake) FailEnsure(err error) { f.mu.Lock(); f.ensureErr = err; f.mu.Unlock() }

// FailConnect makes Connect return err until it is called with nil.
func (f *Fake) FailConnect(err error) { f.mu.Lock(); f.connectErr = err; f.mu.Unlock() }

// FailPhases makes Phases return err until it is called with nil.
func (f *Fake) FailPhases(err error) { f.mu.Lock(); f.phasesErr = err; f.mu.Unlock() }

// SetPhase sets the sandbox's phase in memory, or removes it when phase is empty.
func (f *Fake) SetPhase(name string, phase sandbox.Phase) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if phase == "" {
		delete(f.phase, name)
		return
	}
	f.phase[name] = string(phase)
}

// PhasesCalls returns how many times Phases was called.
func (f *Fake) PhasesCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.phasesCalls
}

// WithoutRPCs makes the worker answer the named RPCs, such as "ReadDir", as a worker built before them would.
func (f *Fake) WithoutRPCs(names ...string) {
	f.mu.Lock()
	f.missing = append(f.missing, names...)
	f.mu.Unlock()
}

// Counts returns how many times Ensure, Stop and Delete were called.
func (f *Fake) Counts() (ensures, stops, deletes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ensures, f.stops, f.deletes
}

// StdioStarts returns how many Stdio streams workers have been asked to open, so tests can tell a process the
// worker started from one started on the host.
func (f *Fake) StdioStarts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stdios
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

// Phases returns a snapshot of every sandbox's neutral lifecycle phase.
func (f *Fake) Phases(ctx context.Context) (map[string]sandbox.Phase, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.phasesCalls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.phasesErr != nil {
		return nil, f.phasesErr
	}
	out := make(map[string]sandbox.Phase, len(f.phase))
	for name, p := range f.phase {
		if p == Ready {
			out[name] = sandbox.PhaseRunning
			continue
		}
		out[name] = sandbox.Phase(p)
	}
	return out, nil
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
	// A deleted sandbox loses its filesystem; a stopped one keeps it.
	delete(f.roots, name)
	return nil
}

// Root returns the workspace root of the named sandbox's worker, creating it on first use.
func (f *Fake) Root(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.roots[name]; ok {
		return r
	}
	r := f.t.TempDir()
	f.roots[name] = r
	return r
}

func (f *Fake) Connect(_ context.Context, name, token string) (*sandbox.Client, io.Closer, error) {
	f.mu.Lock()
	err := f.connectErr
	f.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	root := f.Root(name)
	all, err := tools.All(root, f.t.TempDir())
	require.NoError(f.t, err)
	h, err := worker.Handler(worker.Config{Tools: all, Root: root, Token: token})
	require.NoError(f.t, err)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Stdio") {
			f.mu.Lock()
			f.stdios++
			f.mu.Unlock()
		}
		f.mu.Lock()
		missing := slices.Contains(f.missing, path.Base(r.URL.Path))
		f.mu.Unlock()
		if missing {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	}))
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
