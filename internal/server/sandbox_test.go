package server

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/api/durable/v1/durablev1connect"
	"github.com/wolfeidau/durable/internal/commands"
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
	req := connect.NewRequest(&v1.CreateSessionRequest{Cwd: t.TempDir(), Name: "sb"})
	req.Header().Set("Authorization", "Bearer "+testToken)
	return e.client.CreateSession(t.Context(), req)
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
