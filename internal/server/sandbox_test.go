package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
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

	v1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/api/durable/v1/durablev1connect"
	"github.com/wolfeidau/durable/internal/commands"
	"github.com/wolfeidau/durable/internal/githost"
	"github.com/wolfeidau/durable/internal/githost/githosttest"
	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/sandbox/sandboxtest"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type sandboxEnv struct {
	*registryEnv
	fake   *sandboxtest.Fake
	host   *githosttest.Host
	reg    *Registry
	client durablev1connect.SessionServiceClient
	log    *lockedBuffer
}

func newSandboxEnv(t *testing.T) *sandboxEnv {
	t.Helper()
	e := newEnv(t)
	f := sandboxtest.New(t)
	h := githosttest.New(t)
	e.cfg.Sandbox = &commands.SandboxConfig{
		Backend: f, Environments: map[string]commands.Environment{"default": {Image: "img:1"}},
		GitHosts: map[string]githost.Host{"github": h}, GitTokens: map[string]bool{"github": true},
	}
	e.cfg.IdleTimeout = 100 * time.Millisecond
	log := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(log, nil))
	e.cfg.Logger = logger
	r := e.registry(t)
	draining := make(chan struct{})
	ts := httptest.NewUnstartedServer(nil)
	ts.Config.Handler = New(Config{
		Home: e.home, Token: testToken, Addr: ts.Listener.Addr().String(), Registry: r, Logger: logger,
		Draining: draining, SandboxPhases: f.Phases,
	}).Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(draining) })
	return &sandboxEnv{registryEnv: e, fake: f, host: h, reg: r, client: durablev1connect.NewSessionServiceClient(http.DefaultClient, ts.URL), log: log}
}

func (e *sandboxEnv) listSessions(t *testing.T, refresh bool) *connect.Response[v1.ListSessionsResponse] {
	t.Helper()
	req := connect.NewRequest(&v1.ListSessionsRequest{RefreshSandboxStatus: refresh})
	req.Header().Set("Authorization", "Bearer "+testToken)
	res, err := e.client.ListSessions(t.Context(), req)
	require.NoError(t, err)
	return res
}

func (e *sandboxEnv) createSession(t *testing.T) (*connect.Response[v1.CreateSessionResponse], error) {
	t.Helper()
	return e.create(t, &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_EMPTY, Name: "sb", RequestId: "req-0123456789abcdef"})
}

func (e *sandboxEnv) create(t *testing.T, msg *v1.CreateSessionRequest) (*connect.Response[v1.CreateSessionResponse], error) {
	t.Helper()
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+testToken)
	return e.client.CreateSession(t.Context(), req)
}

func (e *sandboxEnv) info(t *testing.T) *connect.Response[v1.GetServerInfoResponse] {
	t.Helper()
	req := connect.NewRequest(&v1.GetServerInfoRequest{})
	req.Header().Set("Authorization", "Bearer "+testToken)
	res, err := e.client.GetServerInfo(t.Context(), req)
	require.NoError(t, err)
	return res
}

func TestEmptyCreateIsIdempotentByRequestID(t *testing.T) {
	e := newSandboxEnv(t)

	first, err := e.create(t, &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_EMPTY, Name: "one", RequestId: "req-r1-0123456789"})
	require.NoError(t, err)
	require.True(t, first.Msg.Created)
	require.Equal(t, v1.Workspace_WORKSPACE_EMPTY, first.Msg.Session.Workspace)
	require.Empty(t, first.Msg.Session.Cwd)
	ensures, _, _ := e.fake.Counts()
	require.Equal(t, 1, ensures)

	retry, err := e.create(t, &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_EMPTY, Name: "other", RequestId: "req-r1-0123456789"})
	require.NoError(t, err)
	require.False(t, retry.Msg.Created)
	require.Equal(t, first.Msg.Session.Id, retry.Msg.Session.Id)
	require.Equal(t, "one", retry.Msg.Session.Name)
	ensures, _, _ = e.fake.Counts()
	require.Equal(t, 1, ensures, "a retry doesn't create another sandbox")

	next, err := e.create(t, &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_EMPTY, RequestId: "req-r2-0123456789"})
	require.NoError(t, err)
	require.True(t, next.Msg.Created)
	require.NotEqual(t, first.Msg.Session.Id, next.Msg.Session.Id)
}

func TestConcurrentEmptyCreatesWithOneRequestIDMakeOneSession(t *testing.T) {
	e := newSandboxEnv(t)

	const n = 6
	type result struct {
		id      string
		created bool
		err     error
	}
	results := make(chan result, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			res, err := e.create(t, &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_EMPTY, RequestId: "same-0123456789abcd"})
			if err != nil {
				results <- result{err: err}
				return
			}
			results <- result{id: res.Msg.Session.Id, created: res.Msg.Created}
		})
	}
	wg.Wait()
	close(results)

	created := 0
	var id string
	for r := range results {
		require.NoError(t, r.err)
		if id == "" {
			id = r.id
		}
		require.Equal(t, id, r.id)
		if r.created {
			created++
		}
	}
	require.Equal(t, 1, created)
	ensures, _, _ := e.fake.Counts()
	require.Equal(t, 1, ensures)
	require.Equal(t, commands.EmptySessionID("same-0123456789abcd"), id)
}

func TestListSessionsReportsEmptyWorkspace(t *testing.T) {
	e := newSandboxEnv(t)
	_, err := e.createSession(t)
	require.NoError(t, err)

	req := connect.NewRequest(&v1.ListSessionsRequest{})
	req.Header().Set("Authorization", "Bearer "+testToken)
	res, err := e.client.ListSessions(t.Context(), req)
	require.NoError(t, err)
	var empty []*v1.SessionInfo
	for _, s := range res.Msg.Sessions {
		if s.Workspace == v1.Workspace_WORKSPACE_EMPTY {
			empty = append(empty, s)
		}
	}
	require.Len(t, empty, 1)
	require.Empty(t, empty[0].Cwd)
}

func TestEmptyCreateRejectsBadRequests(t *testing.T) {
	e := newSandboxEnv(t)
	tests := []struct {
		name string
		msg  *v1.CreateSessionRequest
		code connect.Code
	}{
		{"directory on a sandboxed server", &v1.CreateSessionRequest{Cwd: t.TempDir()}, connect.CodeFailedPrecondition},
		{"empty with a cwd", &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_EMPTY, Cwd: t.TempDir(), RequestId: "r-0123456789abcdef"}, connect.CodeInvalidArgument},
		{"empty without a request id", &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_EMPTY}, connect.CodeInvalidArgument},
		{"empty with a short request id", &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_EMPTY, RequestId: "short"}, connect.CodeInvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.create(t, tc.msg)
			require.Equal(t, tc.code, connect.CodeOf(err))
		})
	}
	ensures, _, _ := e.fake.Counts()
	require.Zero(t, ensures)
}

func TestEmptyCreateOnPlainServerIsRefused(t *testing.T) {
	e := newEnv(t)
	r := e.registry(t)
	svc := sessionService{reg: r, home: e.home, logger: slog.New(slog.DiscardHandler)}

	_, err := svc.CreateSession(t.Context(), connect.NewRequest(&v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_EMPTY, RequestId: "r-0123456789abcdef"}))
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))

	info, err := svc.GetServerInfo(t.Context(), connect.NewRequest(&v1.GetServerInfoRequest{}))
	require.NoError(t, err)
	require.Equal(t, []v1.Workspace{v1.Workspace_WORKSPACE_DIRECTORY}, info.Msg.Workspaces)
}

func TestGetServerInfoOnSandboxedServer(t *testing.T) {
	e := newSandboxEnv(t)
	require.Equal(t, []v1.Workspace{v1.Workspace_WORKSPACE_EMPTY, v1.Workspace_WORKSPACE_REPO}, e.info(t).Msg.Workspaces)
}

func TestEmptySessionReopensAfterIdleClose(t *testing.T) {
	e := newSandboxEnv(t)
	res, err := e.createSession(t)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return e.open(e.reg) == 0 }, 10*time.Second, 10*time.Millisecond)

	s, release, err := e.reg.Acquire(t.Context(), res.Msg.Session.Id)
	require.NoError(t, err)
	defer release()
	require.NotNil(t, s)
}

func TestIdleSweepStopsSandboxAndReopenEnsuresAgain(t *testing.T) {
	e := newSandboxEnv(t)

	res, err := e.createSession(t)
	require.NoError(t, err)
	require.True(t, res.Msg.Created)
	ensures, _, _ := e.fake.Counts()
	require.Equal(t, 1, ensures)

	require.Eventually(t, func() bool {
		_, stops, _ := e.fake.Counts()
		return stops == 1
	}, 10*time.Second, 10*time.Millisecond)

	_, release, err := e.reg.Acquire(t.Context(), res.Msg.Session.Id)
	require.NoError(t, err)
	defer release()
	ensures, _, _ = e.fake.Counts()
	require.Equal(t, 2, ensures)
}

func TestCreateMapsSandboxErrorsToFixedMessages(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code connect.Code
		msg  string
	}{
		{name: "unavailable", err: fmt.Errorf("gateway at 10.0.0.1 down: %w", sandboxtest.ErrUnavailable), code: connect.CodeUnavailable, msg: "sandbox unavailable: see the server log"},
		{name: "configuration", err: errors.New("image registry.example/secret-name not found"), code: connect.CodeFailedPrecondition, msg: "sandbox can't start: see the server log"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newSandboxEnv(t)
			e.fake.FailEnsure(tc.err)

			_, err := e.createSession(t)

			require.Equal(t, tc.code, connect.CodeOf(err))
			var ce *connect.Error
			require.ErrorAs(t, err, &ce)
			require.Equal(t, tc.msg, ce.Message())
			require.Contains(t, e.log.String(), tc.err.Error()[:20])
		})
	}
}

func TestReopenMapsSandboxErrorsToFixedMessages(t *testing.T) {
	e := newSandboxEnv(t)
	res, err := e.createSession(t)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return e.open(e.reg) == 0 }, 10*time.Second, 10*time.Millisecond)

	e.fake.FailEnsure(fmt.Errorf("gateway down: %w", sandboxtest.ErrUnavailable))
	req := connect.NewRequest(&v1.SubmitRequest{SessionId: res.Msg.Session.Id, RequestId: "r1", Text: "hi"})
	req.Header().Set("Authorization", "Bearer "+testToken)
	_, err = e.client.Submit(t.Context(), req)

	require.Equal(t, connect.CodeUnavailable, connect.CodeOf(err))
	var ce *connect.Error
	require.ErrorAs(t, err, &ce)
	require.Equal(t, "sandbox unavailable: see the server log", ce.Message())
	require.Contains(t, e.log.String(), "gateway down")
}

func TestFailedFirstEmptyCreateDeletesTheSandboxAndRetryCreatesItFresh(t *testing.T) {
	e := newSandboxEnv(t)
	const requestID = "req-retry-0123456789"
	id := commands.EmptySessionID(requestID)
	dir := filepath.Join(e.home, ".durable", "sessions", id)
	msg := &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_EMPTY, RequestId: requestID}

	e.fake.FailConnect(errors.New("catalog mismatch"))
	_, err := e.create(t, msg)
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	ensures, stops, deletes := e.fake.Counts()
	require.Equal(t, 1, ensures)
	require.Zero(t, stops)
	require.Equal(t, 1, deletes)
	pin, ok, err := sandbox.ReadPin(dir)
	require.NoError(t, err)
	require.True(t, ok, "the pin survives so the token does")
	require.Empty(t, e.fake.Phase(pin.Name))

	e.fake.FailConnect(nil)
	res, err := e.create(t, msg)
	require.NoError(t, err)
	require.True(t, res.Msg.Created)
	require.Equal(t, id, res.Msg.Session.Id)
	again, ok, err := sandbox.ReadPin(dir)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, pin.Token, again.Token)
	ensures, _, _ = e.fake.Counts()
	require.Equal(t, 2, ensures)
}

func TestUnnamedEmptySessionHasNoName(t *testing.T) {
	e := newSandboxEnv(t)

	res, err := e.create(t, &v1.CreateSessionRequest{Workspace: v1.Workspace_WORKSPACE_EMPTY, RequestId: "req-noname-0123456789"})

	require.NoError(t, err)
	require.Empty(t, res.Msg.Session.Name)
}

func TestEmptySessionWithoutPinReopensAsFailedPrecondition(t *testing.T) {
	e := newSandboxEnv(t)
	res, err := e.createSession(t)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return e.open(e.reg) == 0 }, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, os.Remove(filepath.Join(e.home, ".durable", "sessions", res.Msg.Session.Id, sandbox.PinFile)))

	req := connect.NewRequest(&v1.SubmitRequest{SessionId: res.Msg.Session.Id, RequestId: "r1", Text: "hi"})
	req.Header().Set("Authorization", "Bearer "+testToken)
	_, err = e.client.Submit(t.Context(), req)

	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	var ce *connect.Error
	require.ErrorAs(t, err, &ce)
	require.Equal(t, "this session has no sandbox pin: see the server log", ce.Message())
}

func TestListSessionsSandboxLifecycleAndIdleSweep(t *testing.T) {
	e := newEnv(t)
	f := sandboxtest.New(t)
	h := githosttest.New(t)
	e.cfg.Sandbox = &commands.SandboxConfig{
		Backend: f, Environments: map[string]commands.Environment{"default": {Image: "img:1"}},
		GitHosts: map[string]githost.Host{"github": h}, GitTokens: map[string]bool{"github": true},
	}
	e.cfg.IdleTimeout = 100 * time.Millisecond
	log := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(log, nil))
	e.cfg.Logger = logger
	r := e.registry(t)

	// Gate the first Phases call so ColdRefreshing is observed deterministically.
	firstStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	var firstDone atomic.Bool
	phasesFn := func(ctx context.Context) (map[string]sandbox.Phase, error) {
		if firstDone.CompareAndSwap(false, true) {
			firstStarted <- struct{}{}
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return f.Phases(ctx)
	}

	draining := make(chan struct{})
	ts := httptest.NewUnstartedServer(nil)
	ts.Config.Handler = New(Config{
		Home: e.home, Token: testToken, Addr: ts.Listener.Addr().String(), Registry: r, Logger: logger,
		Draining: draining, SandboxPhases: phasesFn,
	}).Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(draining) })
	se := &sandboxEnv{registryEnv: e, fake: f, host: h, reg: r, client: durablev1connect.NewSessionServiceClient(http.DefaultClient, ts.URL), log: log}

	created, err := se.createSession(t)
	require.NoError(t, err)
	id := created.Msg.Session.Id
	pin, ok, err := sandbox.ReadPin(filepath.Join(e.home, ".durable", "sessions", id))
	require.NoError(t, err)
	require.True(t, ok)

	// Hold the session open while checking cold -> running so idle sweep does not race ahead.
	_, releaseHold, err := r.Acquire(t.Context(), id)
	require.NoError(t, err)

	findSession := func(sessions []*v1.SessionInfo, wantID string) *v1.SessionInfo {
		for _, s := range sessions {
			if s.Id == wantID {
				return s
			}
		}
		return nil
	}

	// Cold first ListSessions returns immediately with UNKNOWN + refreshing: true.
	cold := se.listSessions(t, false)
	require.Len(t, cold.Msg.Sessions, 2)
	require.Equal(t, v1.SandboxStatus_SANDBOX_STATUS_UNSPECIFIED, findSession(cold.Msg.Sessions, e.id).SandboxStatus)
	require.Equal(t, v1.SandboxStatus_SANDBOX_STATUS_UNKNOWN, findSession(cold.Msg.Sessions, id).SandboxStatus)
	require.NotNil(t, cold.Msg.SandboxSnapshot)
	require.Nil(t, cold.Msg.SandboxSnapshot.ObservedAt)
	require.True(t, cold.Msg.SandboxSnapshot.Refreshing)
	require.True(t, cold.Msg.SandboxSnapshot.Stale)
	require.Empty(t, cold.Msg.SandboxSnapshot.Error)

	<-firstStarted
	close(releaseFirst)

	// Once the first refresh finishes, status settles to RUNNING.
	require.Eventually(t, func() bool {
		res := se.listSessions(t, false)
		snap := res.Msg.SandboxSnapshot
		return snap != nil && snap.ObservedAt != nil && !snap.Refreshing && !snap.Stale &&
			findSession(res.Msg.Sessions, id).SandboxStatus == v1.SandboxStatus_SANDBOX_STATUS_RUNNING
	}, 5*time.Second, 10*time.Millisecond)

	// Releasing the hold lets the idle sweep stop the sandbox and invalidate the cache.
	releaseHold()
	require.Eventually(t, func() bool {
		res := se.listSessions(t, false)
		snap := res.Msg.SandboxSnapshot
		return snap != nil && !snap.Refreshing && !snap.Stale &&
			findSession(res.Msg.Sessions, id).SandboxStatus == v1.SandboxStatus_SANDBOX_STATUS_STOPPED
	}, 10*time.Second, 20*time.Millisecond)

	// Verify remaining phase mappings via force refresh.
	for phase, want := range map[sandbox.Phase]v1.SandboxStatus{
		sandbox.PhaseStarting: v1.SandboxStatus_SANDBOX_STATUS_STARTING,
		sandbox.PhaseStopping: v1.SandboxStatus_SANDBOX_STATUS_STOPPING,
		sandbox.PhaseError:    v1.SandboxStatus_SANDBOX_STATUS_ERROR,
		sandbox.PhaseUnknown:  v1.SandboxStatus_SANDBOX_STATUS_UNKNOWN,
	} {
		f.SetPhase(pin.Name, phase)
		_ = se.listSessions(t, true)
		require.Eventually(t, func() bool {
			res := se.listSessions(t, false)
			return !res.Msg.SandboxSnapshot.Refreshing && findSession(res.Msg.Sessions, id).SandboxStatus == want
		}, 5*time.Second, 10*time.Millisecond, "phase %q", phase)
	}

	// Deleting the sandbox causes an observed snapshot omitting it to report MISSING.
	f.SetPhase(pin.Name, "")
	_ = se.listSessions(t, true)
	require.Eventually(t, func() bool {
		res := se.listSessions(t, false)
		snap := res.Msg.SandboxSnapshot
		return snap != nil && snap.ObservedAt != nil && !snap.Refreshing && !snap.Stale &&
			findSession(res.Msg.Sessions, id).SandboxStatus == v1.SandboxStatus_SANDBOX_STATUS_MISSING
	}, 5*time.Second, 10*time.Millisecond)
}

func TestListSessionsSandboxGatewayFailurePreservesCachedAndHidesRawError(t *testing.T) {
	e := newSandboxEnv(t)
	created, err := e.createSession(t)
	require.NoError(t, err)
	id := created.Msg.Session.Id
	_, releaseHold, err := e.reg.Acquire(t.Context(), id)
	require.NoError(t, err)
	defer releaseHold()

	find := func(sessions []*v1.SessionInfo) *v1.SessionInfo {
		for _, s := range sessions {
			if s.Id == id {
				return s
			}
		}
		return nil
	}

	require.Eventually(t, func() bool {
		res := e.listSessions(t, false)
		snap := res.Msg.SandboxSnapshot
		return snap != nil && snap.ObservedAt != nil && !snap.Refreshing && !snap.Stale &&
			find(res.Msg.Sessions).SandboxStatus == v1.SandboxStatus_SANDBOX_STATUS_RUNNING
	}, 5*time.Second, 10*time.Millisecond)

	// Transient gateway error preserves cached RUNNING status and hides the raw cause from clients.
	e.fake.FailPhases(errors.New("secret-gateway-10.9.8.7 refused"))
	_ = e.listSessions(t, true)
	var failed *v1.ListSessionsResponse
	require.Eventually(t, func() bool {
		res := e.listSessions(t, false)
		if !res.Msg.SandboxSnapshot.Refreshing && res.Msg.SandboxSnapshot.Error != "" {
			failed = res.Msg
			return true
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)

	require.Equal(t, v1.SandboxStatus_SANDBOX_STATUS_RUNNING, find(failed.Sessions).SandboxStatus)
	require.NotNil(t, failed.SandboxSnapshot.ObservedAt)
	require.True(t, failed.SandboxSnapshot.Stale)
	require.Equal(t, "sandbox status could not be refreshed: see the server log", failed.SandboxSnapshot.Error)
	require.NotContains(t, failed.SandboxSnapshot.Error, "secret-gateway-10.9.8.7")
	require.Contains(t, e.log.String(), "secret-gateway-10.9.8.7")

	// Clearing the gateway error recovers cleanly on refresh.
	e.fake.FailPhases(nil)
	_ = e.listSessions(t, true)
	require.Eventually(t, func() bool {
		res := e.listSessions(t, false)
		snap := res.Msg.SandboxSnapshot
		return !snap.Refreshing && !snap.Stale && snap.Error == ""
	}, 5*time.Second, 10*time.Millisecond)
}

func TestListSessionsSandboxColdFailureReportsGenericUnavailable(t *testing.T) {
	e := newSandboxEnv(t)
	e.fake.FailPhases(errors.New("secret-cold-gateway-down"))
	created, err := e.createSession(t)
	require.NoError(t, err)
	id := created.Msg.Session.Id

	_ = e.listSessions(t, false)
	var failed *v1.ListSessionsResponse
	require.Eventually(t, func() bool {
		res := e.listSessions(t, false)
		snap := res.Msg.SandboxSnapshot
		if snap != nil && !snap.Refreshing && snap.Error != "" {
			failed = res.Msg
			return true
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)

	var got *v1.SessionInfo
	for _, s := range failed.Sessions {
		if s.Id == id {
			got = s
		}
	}
	require.NotNil(t, got)
	require.Equal(t, v1.SandboxStatus_SANDBOX_STATUS_UNKNOWN, got.SandboxStatus)
	require.Nil(t, failed.SandboxSnapshot.ObservedAt)
	require.True(t, failed.SandboxSnapshot.Stale)
	require.Equal(t, "sandbox status unavailable: see the server log", failed.SandboxSnapshot.Error)
	require.NotContains(t, failed.SandboxSnapshot.Error, "secret-cold-gateway-down")
	require.Contains(t, e.log.String(), "secret-cold-gateway-down")
}

func TestListSessionsDirectoryOnlySkipsSandboxPhases(t *testing.T) {
	e := newEnv(t)
	id := e.id
	r := e.registry(t)

	var calls atomic.Int64
	draining := make(chan struct{})
	ts := httptest.NewUnstartedServer(nil)
	ts.Config.Handler = New(Config{
		Home: e.home, Token: testToken, Addr: ts.Listener.Addr().String(), Registry: r,
		Draining: draining,
		SandboxPhases: func(context.Context) (map[string]sandbox.Phase, error) {
			calls.Add(1)
			return map[string]sandbox.Phase{}, nil
		},
	}).Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(draining) })
	client := durablev1connect.NewSessionServiceClient(http.DefaultClient, ts.URL)

	for _, refresh := range []bool{false, true} {
		req := connect.NewRequest(&v1.ListSessionsRequest{RefreshSandboxStatus: refresh})
		req.Header().Set("Authorization", "Bearer "+testToken)
		res, err := client.ListSessions(t.Context(), req)
		require.NoError(t, err)
		require.Empty(t, res.Msg.Warnings)
		require.Nil(t, res.Msg.SandboxSnapshot)
		require.Len(t, res.Msg.Sessions, 1)
		require.Equal(t, id, res.Msg.Sessions[0].Id)
		require.Equal(t, v1.SandboxStatus_SANDBOX_STATUS_UNSPECIFIED, res.Msg.Sessions[0].SandboxStatus)
	}
	require.Zero(t, calls.Load())
}

func TestListSessionsCorruptSandboxPinWarns(t *testing.T) {
	e := newEnv(t)
	id := e.id
	require.NoError(t, os.WriteFile(filepath.Join(e.home, ".durable", "sessions", id, sandbox.PinFile), []byte("{not-json"), 0o600))
	r := e.registry(t)

	var calls atomic.Int64
	draining := make(chan struct{})
	ts := httptest.NewUnstartedServer(nil)
	ts.Config.Handler = New(Config{
		Home: e.home, Token: testToken, Addr: ts.Listener.Addr().String(), Registry: r,
		Draining: draining,
		SandboxPhases: func(context.Context) (map[string]sandbox.Phase, error) {
			calls.Add(1)
			return map[string]sandbox.Phase{}, nil
		},
	}).Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(draining) })
	client := durablev1connect.NewSessionServiceClient(http.DefaultClient, ts.URL)

	req := connect.NewRequest(&v1.ListSessionsRequest{})
	req.Header().Set("Authorization", "Bearer "+testToken)
	res, err := client.ListSessions(t.Context(), req)
	require.NoError(t, err)
	require.Contains(t, res.Msg.Warnings, "sandbox status unavailable for "+id)
	require.Nil(t, res.Msg.SandboxSnapshot)
	require.Len(t, res.Msg.Sessions, 1)
	require.Equal(t, v1.SandboxStatus_SANDBOX_STATUS_UNSPECIFIED, res.Msg.Sessions[0].SandboxStatus)
	require.Zero(t, calls.Load())
}

func TestListSessionsPinnedSessionWithNilSandboxPhasesReportsUnknown(t *testing.T) {
	e := newEnv(t)
	f := sandboxtest.New(t)
	h := githosttest.New(t)
	e.cfg.Sandbox = &commands.SandboxConfig{
		Backend: f, Environments: map[string]commands.Environment{"default": {Image: "img:1"}},
		GitHosts: map[string]githost.Host{"github": h}, GitTokens: map[string]bool{"github": true},
	}
	r := e.registry(t)
	ts := httptest.NewUnstartedServer(nil)
	ts.Config.Handler = New(Config{
		Home: e.home, Token: testToken, Addr: ts.Listener.Addr().String(), Registry: r,
	}).Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	se := &sandboxEnv{registryEnv: e, fake: f, host: h, reg: r, client: durablev1connect.NewSessionServiceClient(http.DefaultClient, ts.URL)}

	created, err := se.createSession(t)
	require.NoError(t, err)
	res := se.listSessions(t, false)
	require.Nil(t, res.Msg.SandboxSnapshot)
	require.Len(t, res.Msg.Sessions, 2)
	byID := map[string]*v1.SessionInfo{}
	for _, s := range res.Msg.Sessions {
		byID[s.Id] = s
	}
	require.Equal(t, v1.SandboxStatus_SANDBOX_STATUS_UNSPECIFIED, byID[e.id].SandboxStatus)
	require.Equal(t, v1.SandboxStatus_SANDBOX_STATUS_UNKNOWN, byID[created.Msg.Session.Id].SandboxStatus)
}
