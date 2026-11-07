package server

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	v1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/internal/commands"
)

type outageModel struct{ calls atomic.Int32 }

func (m *outageModel) Generate(context.Context, []durable.Message, []durable.ToolSpec) (durable.Message, error) {
	m.calls.Add(1)
	return durable.Message{}, durable.Transient(errors.New("outage"))
}

// gateModel holds its first call until proceed closes, then answers.
type gateModel struct {
	calls            atomic.Int32
	started, proceed chan struct{}
}

func (m *gateModel) Generate(context.Context, []durable.Message, []durable.ToolSpec) (durable.Message, error) {
	if m.calls.Add(1) == 1 {
		close(m.started)
		<-m.proceed
	}
	return durable.Message{Role: durable.RoleAssistant, Text: "hi"}, nil
}

// useModel makes every session the registry opens run on m.
func (e *registryEnv) useModel(m durable.Model) {
	e.cfg.assemble = func(ctx context.Context, c commands.AssembleConfig) (*commands.Assembled, error) {
		c.APIToken = "k"
		a, err := commands.Assemble(ctx, c)
		if err == nil {
			a.Options.Model = m
			a.Options.ModelAttempts = 1
		}
		return a, err
	}
}

// openPlain opens the session outside the registry, which only succeeds once the registry has released the lease.
func (e *registryEnv) openPlain(t *testing.T) *durable.Harness {
	t.Helper()
	a, err := commands.Assemble(t.Context(), commands.AssembleConfig{CWD: sessionCWD(t, e), Session: "s", APIToken: "k", ExistingOnly: true})
	require.NoError(t, err)
	t.Cleanup(a.Close)
	a.Options.Model = echoModel{}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	h, err := durable.Open(ctx, a.Path, a.Options)
	require.NoError(t, err)
	return h
}

// stealLease rewrites the lease row through a separate connection, as another process would.
func stealLease(t *testing.T, s *Session, expiresAt int64) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+s.asm.Path+"?_busy_timeout=5000")
	require.NoError(t, err)
	defer db.Close()
	_, err = db.ExecContext(t.Context(), `UPDATE lease SET owner = 'thief', expires_at = ?`, expiresAt)
	require.NoError(t, err)
}

func TestOutageClosesIdleSession(t *testing.T) {
	e := newEnv(t)
	e.cfg.IdleTimeout = 200 * time.Millisecond
	e.cfg.RetryBase = 10 * time.Millisecond
	e.cfg.RetryCap = 10 * time.Millisecond
	model := &outageModel{}
	e.useModel(model)
	r := e.registry(t)

	s, release, err := r.Acquire(t.Context(), e.id)
	require.NoError(t, err)
	_, err = s.Harness().Submit(t.Context(), "r1", "hello")
	require.NoError(t, err)
	s.Kick()

	// Held, so the outage retries past the idle timeout without closing.
	time.Sleep(500 * time.Millisecond)
	require.Equal(t, 1, e.open(r))
	require.Greater(t, model.calls.Load(), int32(2))
	release()

	require.Eventually(t, func() bool { return e.open(r) == 0 }, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, e.openPlain(t).Close())
}

func TestLostLeaseStopsDriverAndEvicts(t *testing.T) {
	e := newEnv(t)
	e.cfg.OpenTimeout = 200 * time.Millisecond
	e.cfg.RetryBase = 10 * time.Millisecond
	model := &gateModel{started: make(chan struct{}), proceed: make(chan struct{})}
	e.useModel(model)
	r := e.registry(t)

	s, release, err := r.Acquire(t.Context(), e.id)
	require.NoError(t, err)
	_, err = s.Harness().Submit(t.Context(), "r1", "hello")
	require.NoError(t, err)
	s.Kick()
	<-model.started
	stealLease(t, s, time.Now().Add(time.Hour).UnixMilli())
	close(model.proceed) // the answer's commit hits the fence

	require.Eventually(t, s.lost, 10*time.Second, 5*time.Millisecond)
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		t.Fatal("driver kept running after the lease was lost")
	}
	time.Sleep(100 * time.Millisecond)
	require.EqualValues(t, 1, model.calls.Load())

	_, _, err = r.Acquire(t.Context(), e.id)
	require.Equal(t, connect.CodeUnavailable, connect.CodeOf(err))
	require.ErrorContains(t, err, "session in use")
	require.Zero(t, e.open(r))

	// The old session stays readable until its last holder leaves.
	_, err = s.Harness().Events(t.Context(), 0, 1)
	require.NoError(t, err)
	release()
	_, err = s.Harness().Events(t.Context(), 0, 1)
	require.Error(t, err)

	stealLease(t, s, 0)
	s2, release2, err := r.Acquire(t.Context(), e.id)
	require.NoError(t, err)
	defer release2()
	require.NotSame(t, s, s2)
}

func TestLostLeaseMapsCancelAndDecideToUnavailable(t *testing.T) {
	for _, op := range []string{"cancel", "decide"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			e.cfg.OpenTimeout = 200 * time.Millisecond
			client, r := e.watchClient(t)
			s, release, err := r.Acquire(t.Context(), e.id)
			require.NoError(t, err)
			defer release()
			// Submitted without a kick so it stays queued and Cancel reaches the fence.
			_, err = s.Harness().Submit(t.Context(), "r1", "hi")
			require.NoError(t, err)
			// The opening run would otherwise notice the loss first and the client would see a fresh open fail.
			require.Eventually(t, func() bool {
				s.mu.Lock()
				defer s.mu.Unlock()
				return !s.kicked && !s.running
			}, 10*time.Second, time.Millisecond)
			stealLease(t, s, time.Now().Add(time.Hour).UnixMilli())

			if op == "cancel" {
				req := connect.NewRequest(&v1.CancelRequest{SessionId: e.id, RequestId: "r1"})
				req.Header().Set("Authorization", "Bearer "+testToken)
				_, err = client.Cancel(t.Context(), req)
			} else {
				req := connect.NewRequest(&v1.DecideApprovalRequest{SessionId: e.id, ApprovalSeq: 1, Allow: true, By: "me"})
				req.Header().Set("Authorization", "Bearer "+testToken)
				_, err = client.DecideApproval(t.Context(), req)
			}
			require.Equal(t, connect.CodeUnavailable, connect.CodeOf(err), "%v", err)
			require.ErrorContains(t, err, "session taken over by another process")
			require.True(t, s.lost())
		})
	}
}

func TestBackgroundRenewalMarksIdleSessionLost(t *testing.T) {
	e := newEnv(t)
	e.cfg.OpenTimeout = 200 * time.Millisecond
	e.cfg.assemble = func(ctx context.Context, c commands.AssembleConfig) (*commands.Assembled, error) {
		c.APIToken = "k"
		a, err := commands.Assemble(ctx, c)
		if err == nil {
			a.Options.Model = echoModel{}
			a.Options.LeaseTTL = 300 * time.Millisecond
		}
		return a, err
	}
	client, r := e.watchClient(t)
	s, release, err := r.Acquire(t.Context(), e.id)
	require.NoError(t, err)
	defer release()

	watched := make(chan error, 1)
	go func() {
		stream, err := client.WatchEvents(t.Context(), watchReq(e.id))
		if err != nil {
			watched <- err
			return
		}
		for stream.Receive() {
		}
		watched <- stream.Err()
	}()
	require.Eventually(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		en := r.entries[e.id]
		return en != nil && en.holders == 2
	}, 10*time.Second, 5*time.Millisecond)
	// Let the first kick's run finish, so only the renewal can notice the loss.
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.kicked && !s.running
	}, 10*time.Second, 5*time.Millisecond)
	stealLease(t, s, time.Now().Add(time.Hour).UnixMilli())

	require.Eventually(t, s.lost, 10*time.Second, 10*time.Millisecond)
	select {
	case werr := <-watched:
		require.Equal(t, connect.CodeUnavailable, connect.CodeOf(werr), "%v", werr)
	case <-time.After(10 * time.Second):
		t.Fatal("watch did not end after the lease was lost")
	}
}

func TestRetryPendingAtCloseDoesNotRun(t *testing.T) {
	e := newEnv(t)
	e.cfg.RetryBase = 200 * time.Millisecond
	model := &outageModel{}
	e.useModel(model)
	r := e.registry(t)

	s, release, err := r.Acquire(t.Context(), e.id)
	require.NoError(t, err)
	// The opening kick must finish first, or it could pick up the submission and run twice.
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.kicked && !s.running
	}, 10*time.Second, time.Millisecond)
	_, err = s.Harness().Submit(t.Context(), "r1", "hello")
	require.NoError(t, err)
	s.Kick()
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return model.calls.Load() == 1 && s.retrying
	}, 10*time.Second, time.Millisecond)

	release()
	r.Close()
	time.Sleep(500 * time.Millisecond)
	require.EqualValues(t, 1, model.calls.Load())
}

func TestUnaryAndWatchMapLostLeaseToUnavailable(t *testing.T) {
	e := newEnv(t)
	e.cfg.OpenTimeout = 200 * time.Millisecond
	client, r := e.watchClient(t)
	s, release, err := r.Acquire(t.Context(), e.id)
	require.NoError(t, err)
	defer release()

	// The client call blocks until the first message, so it runs apart.
	watched := make(chan error, 1)
	go func() {
		stream, err := client.WatchEvents(t.Context(), watchReq(e.id))
		if err != nil {
			watched <- err
			return
		}
		for stream.Receive() {
		}
		watched <- stream.Err()
	}()
	// The stream must hold the session before the loss, or it would try a fresh open.
	require.Eventually(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		en := r.entries[e.id]
		return en != nil && en.holders == 2
	}, 10*time.Second, 5*time.Millisecond)
	stealLease(t, s, time.Now().Add(time.Hour).UnixMilli())

	req := connect.NewRequest(&v1.SubmitRequest{SessionId: e.id, RequestId: "r1", Text: "hi"})
	req.Header().Set("Authorization", "Bearer "+testToken)
	_, err = client.Submit(t.Context(), req)
	require.Equal(t, connect.CodeUnavailable, connect.CodeOf(err), "%v", err)
	require.ErrorContains(t, err, "session taken over by another process")
	require.True(t, s.lost())

	werr := <-watched
	require.Equal(t, connect.CodeUnavailable, connect.CodeOf(werr), "%v", werr)
}
