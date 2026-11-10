package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/commands"
)

type echoModel struct{}

func (echoModel) Generate(_ context.Context, _ []durable.Message, _ []durable.ToolSpec) (durable.Message, error) {
	return durable.Message{Role: durable.RoleAssistant, Text: "hi"}, nil
}

// registryEnv has a temporary HOME, a model-list endpoint that fails, and one existing session.
type registryEnv struct {
	home, id string
	opens    atomic.Int32
	cfg      RegistryConfig
}

func newEnv(t *testing.T) *registryEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	t.Setenv("OPENROUTER_BASE_URL", srv.URL)

	a, err := commands.Assemble(t.Context(), commands.AssembleConfig{CWD: t.TempDir(), Session: "s", APIToken: "k"})
	require.NoError(t, err)
	a.Options.Model = echoModel{}
	h, err := durable.Open(t.Context(), a.Path, a.Options)
	require.NoError(t, err)
	require.NoError(t, h.Close())
	a.Close()

	e := &registryEnv{home: home, id: filepath.Base(filepath.Dir(a.Path))}
	e.cfg = RegistryConfig{
		Home:   home,
		Logger: nil,
		assemble: func(ctx context.Context, c commands.AssembleConfig) (*commands.Assembled, error) {
			e.opens.Add(1)
			c.APIToken = "k"
			a, err := commands.Assemble(ctx, c)
			if err == nil {
				a.Options.Model = echoModel{}
				// No renewal tick in a test's lifetime, so only the RPC under test can notice a stolen lease.
				a.Options.LeaseTTL = time.Hour
			}
			return a, err
		},
	}
	return e
}

func (e *registryEnv) registry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry(context.Background(), e.cfg)
	t.Cleanup(r.Close)
	return r
}

func (e *registryEnv) open(r *Registry) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

func TestConcurrentAcquireSharesOneHarness(t *testing.T) {
	e := newEnv(t)
	r := e.registry(t)

	const n = 8
	sessions := make([]*Session, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			s, release, err := r.Acquire(t.Context(), e.id)
			if err == nil {
				defer release()
			}
			sessions[i] = s
		})
	}
	wg.Wait()

	for _, s := range sessions {
		require.NotNil(t, s)
		require.Same(t, sessions[0], s)
	}
	require.EqualValues(t, 1, e.opens.Load())
}

func TestKickRunsQueuedSubmissionAndWaitWakes(t *testing.T) {
	e := newEnv(t)
	r := e.registry(t)
	s, release, err := r.Acquire(t.Context(), e.id)
	require.NoError(t, err)
	defer release()

	// Wait on a seq nothing has reached honours ctx.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, s.Wait(ctx, 1<<40), context.DeadlineExceeded)

	events, err := s.Harness().Events(t.Context(), 0, 0)
	require.NoError(t, err)
	var after int64
	if len(events) > 0 {
		after = events[len(events)-1].Seq
	}
	woke := make(chan error, 1)
	go func() { woke <- s.Wait(t.Context(), after) }()

	_, err = s.Harness().Submit(t.Context(), "r1", "hello")
	require.NoError(t, err)
	s.Kick()
	s.Kick() // coalesces

	select {
	case err := <-woke:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not wake after a commit")
	}
	require.Eventually(t, func() bool {
		sub, err := s.Harness().Get(t.Context(), "r1")
		return err == nil && sub.Status == durable.StatusDone
	}, 10*time.Second, 10*time.Millisecond)
	require.NotEmpty(t, s.Epoch())
}

func TestOpenResumesUnfinishedWork(t *testing.T) {
	e := newEnv(t)
	// Queue a submission with a harness that never runs it.
	a, err := commands.Assemble(t.Context(), commands.AssembleConfig{CWD: sessionCWD(t, e), Session: "s", APIToken: "k", ExistingOnly: true})
	require.NoError(t, err)
	a.Options.Model = echoModel{}
	h, err := durable.Open(t.Context(), a.Path, a.Options)
	require.NoError(t, err)
	_, err = h.Submit(t.Context(), "r1", "hello")
	require.NoError(t, err)
	require.NoError(t, h.Close())
	a.Close()

	r := e.registry(t)
	s, release, err := r.Acquire(t.Context(), e.id)
	require.NoError(t, err)
	defer release()
	require.Eventually(t, func() bool {
		sub, err := s.Harness().Get(t.Context(), "r1")
		return err == nil && sub.Status == durable.StatusDone
	}, 10*time.Second, 10*time.Millisecond)
}

func sessionCWD(t *testing.T, e *registryEnv) string {
	t.Helper()
	dir, err := SessionDir(e.home, e.id)
	require.NoError(t, err)
	m, err := commands.ReadSessionMeta(dir)
	require.NoError(t, err)
	return m.CWD
}

func TestIdleSessionClosesAndReopens(t *testing.T) {
	e := newEnv(t)
	e.cfg.IdleTimeout = 100 * time.Millisecond
	r := e.registry(t)

	s1, release, err := r.Acquire(t.Context(), e.id)
	require.NoError(t, err)
	// Held, so it survives past the timeout.
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, 1, e.open(r))
	release()

	require.Eventually(t, func() bool { return e.open(r) == 0 }, 10*time.Second, 10*time.Millisecond)

	s2, release, err := r.Acquire(t.Context(), e.id)
	require.NoError(t, err)
	defer release()
	require.NotSame(t, s1, s2)
	require.EqualValues(t, 2, e.opens.Load())
}

func TestAcquireRejectsUnknownAndMalformedIDs(t *testing.T) {
	e := newEnv(t)
	r := e.registry(t)

	_, _, err := r.Acquire(t.Context(), "0123456789abcdef")
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
	_, err = os.Stat(filepath.Join(e.home, ".durable", "sessions", "0123456789abcdef"))
	require.ErrorIs(t, err, os.ErrNotExist)

	for _, id := range []string{"../x", "", "0123456789ABCDEF"} {
		_, _, err := r.Acquire(t.Context(), id)
		require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), id)
	}
	require.Zero(t, e.opens.Load())
	require.Zero(t, e.open(r))
}

func TestCloseStopsSessions(t *testing.T) {
	e := newEnv(t)
	r := e.registry(t)
	_, release, err := r.Acquire(t.Context(), e.id)
	require.NoError(t, err)
	defer release()

	r.Close()
	require.Zero(t, e.open(r))
	_, _, err = r.Acquire(t.Context(), e.id)
	require.ErrorIs(t, err, errRegistryClosed)
}

func TestSlowAssembleIsNotSessionInUse(t *testing.T) {
	e := newEnv(t)
	e.cfg.OpenTimeout = 100 * time.Millisecond
	e.cfg.IdleTimeout = 100 * time.Millisecond
	inner := e.cfg.assemble
	var block atomic.Bool
	e.cfg.assemble = func(ctx context.Context, c commands.AssembleConfig) (*commands.Assembled, error) {
		if block.Load() {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		time.Sleep(300 * time.Millisecond)
		return inner(ctx, c)
	}
	r := e.registry(t)

	// Slower than OpenTimeout with no lease held still opens.
	_, release, err := r.Acquire(t.Context(), e.id)
	require.NoError(t, err)
	release()
	require.Eventually(t, func() bool { return e.open(r) == 0 }, 10*time.Second, 10*time.Millisecond)

	// A stuck Assemble ends with the caller's deadline.
	block.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_, _, err = r.Acquire(ctx, e.id)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotEqual(t, connect.CodeUnavailable, connect.CodeOf(err))
}

type flakyModel struct{ calls atomic.Int32 }

func (m *flakyModel) Generate(context.Context, []durable.Message, []durable.ToolSpec) (durable.Message, error) {
	if m.calls.Add(1) == 1 {
		return durable.Message{}, durable.Transient(errors.New("outage"))
	}
	return durable.Message{Role: durable.RoleAssistant, Text: "hi"}, nil
}

func TestFailedRunIsRetriedWithoutAKick(t *testing.T) {
	e := newEnv(t)
	e.cfg.RetryBase = 20 * time.Millisecond
	model := &flakyModel{}
	e.cfg.assemble = func(ctx context.Context, c commands.AssembleConfig) (*commands.Assembled, error) {
		c.APIToken = "k"
		a, err := commands.Assemble(ctx, c)
		if err == nil {
			a.Options.Model = model
			a.Options.ModelAttempts = 1
		}
		return a, err
	}
	r := e.registry(t)
	s, release, err := r.Acquire(t.Context(), e.id)
	require.NoError(t, err)
	defer release()

	_, err = s.Harness().Submit(t.Context(), "r1", "hello")
	require.NoError(t, err)
	s.Kick()
	require.Eventually(t, func() bool {
		sub, err := s.Harness().Get(t.Context(), "r1")
		return err == nil && sub.Status == durable.StatusDone
	}, 10*time.Second, 10*time.Millisecond)
	require.GreaterOrEqual(t, model.calls.Load(), int32(2))
}

func TestAcquireReportsHeldLeaseAsUnavailable(t *testing.T) {
	e := newEnv(t)
	e.cfg.OpenTimeout = 200 * time.Millisecond
	r := e.registry(t)

	a, err := commands.Assemble(t.Context(), commands.AssembleConfig{CWD: sessionCWD(t, e), Session: "s", APIToken: "k", ExistingOnly: true})
	require.NoError(t, err)
	a.Options.Model = echoModel{}
	holder, err := durable.Open(t.Context(), a.Path, a.Options)
	require.NoError(t, err)
	defer a.Close()

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, _, err := r.Acquire(t.Context(), e.id)
			errs <- err
		}()
	}
	for range 2 {
		err := <-errs
		require.Equal(t, connect.CodeUnavailable, connect.CodeOf(err))
		require.ErrorContains(t, err, "session in use")
	}
	require.Zero(t, e.open(r))

	require.NoError(t, holder.Close())
	_, release, err := r.Acquire(t.Context(), e.id)
	require.NoError(t, err)
	release()
}

func TestConcurrentAcquireAndCreateShareOneSession(t *testing.T) {
	e := newEnv(t)
	r := e.registry(t)
	cwd := t.TempDir()
	path, err := commands.SessionPath(cwd, "new")
	require.NoError(t, err)
	id := filepath.Base(filepath.Dir(path))

	const n = 4
	sessions := make([]*Session, n)
	errs := make(chan error, 2*n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, _, err := r.Create(t.Context(), createSpec{cwd: cwd, name: "new"})
			errs <- err
		})
		wg.Go(func() {
			// Acquire can't open a session that doesn't exist yet, so it retries until Create has made it.
			for {
				s, release, err := r.Acquire(t.Context(), id)
				if err == nil {
					release()
					sessions[i] = s
					return
				}
				if connect.CodeOf(err) != connect.CodeNotFound {
					errs <- err
					return
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	for _, s := range sessions {
		require.NotNil(t, s)
		require.Same(t, sessions[0], s)
	}
	require.EqualValues(t, 1, e.opens.Load())
}

func TestCreateReturnsSessionAnotherProcessCreatedMeanwhile(t *testing.T) {
	e := newEnv(t)
	inner := e.cfg.assemble
	e.cfg.assemble = func(ctx context.Context, c commands.AssembleConfig) (*commands.Assembled, error) {
		if c.ExistingOnly {
			return inner(ctx, c)
		}
		// The CLI creates the session first, then its pin rejects the server's flags.
		a, err := inner(ctx, c)
		require.NoError(t, err)
		h, err := durable.Open(ctx, a.Path, a.Options)
		require.NoError(t, err)
		require.NoError(t, h.Close())
		a.Close()
		return nil, errors.New("--provider openrouter does not match the session's pin")
	}
	r := e.registry(t)
	cwd := t.TempDir()

	meta, created, err := r.Create(t.Context(), createSpec{cwd: cwd, name: "raced"})
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, cwd, meta.CWD)
	require.NotEmpty(t, meta.ID)
}
