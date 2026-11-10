package server

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/api/durable/v1/durablev1connect"
	"github.com/wolfeidau/durable/internal/commands"
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
	reg    *Registry
	client durablev1connect.SessionServiceClient
	log    *lockedBuffer
}

func newSandboxEnv(t *testing.T) *sandboxEnv {
	t.Helper()
	e := newEnv(t)
	f := sandboxtest.New(t)
	e.cfg.Sandbox = &commands.SandboxConfig{Backend: f, Image: "img:1"}
	e.cfg.IdleTimeout = 100 * time.Millisecond
	log := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(log, nil))
	e.cfg.Logger = logger
	r := e.registry(t)
	ts := httptest.NewUnstartedServer(nil)
	ts.Config.Handler = New(Config{Home: e.home, Token: testToken, Addr: ts.Listener.Addr().String(), Registry: r, Logger: logger}).Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	return &sandboxEnv{registryEnv: e, fake: f, reg: r, client: durablev1connect.NewSessionServiceClient(http.DefaultClient, ts.URL), log: log}
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
	require.Equal(t, []v1.Workspace{v1.Workspace_WORKSPACE_EMPTY}, e.info(t).Msg.Workspaces)
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
	require.Equal(t, "this empty session has no sandbox pin: see the server log", ce.Message())
}
