package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/api/durable/v1/durablev1connect"
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

// binPath is the real binary, built once because linking it per test dominated the suite's run time.
var binPath string

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "durable-server-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	binPath = filepath.Join(dir, "durable-server")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to build binary: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

type running struct {
	url, token, home string
	cmd              *exec.Cmd
	done             chan struct{}
}

func start(t *testing.T) *running {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command(binPath, "--addr", "127.0.0.1:0")
	cmd.Env = append(os.Environ(), "HOME="+home)
	require.NoError(t, cmd.Start())
	r := &running{home: home, cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(r.done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-r.done
	})

	infoPath := filepath.Join(home, ".durable", "server.json")
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(infoPath)
		if err != nil {
			return false
		}
		var info struct{ Addr, Token string }
		if json.Unmarshal(data, &info) != nil || info.Addr == "" {
			return false
		}
		r.url, r.token = "http://"+info.Addr, info.Token
		return true
	}, 10*time.Second, 20*time.Millisecond)
	return r
}

// startServer starts the binary on a free port and returns its base URL and token.
func startServer(t *testing.T) (baseURL, token string) {
	t.Helper()
	r := start(t)
	return r.url, r.token
}

func h2cClient() *http.Client {
	tr := &http.Transport{Protocols: new(http.Protocols)}
	tr.Protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: tr}
}

func TestRejectsBadRequests(t *testing.T) {
	base, token := startServer(t)
	path := base + durablev1connect.SessionServiceSubmitProcedure

	do := func(mutate func(*http.Request)) (status int, cors string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, path, nil)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		mutate(req)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin")
	}

	status, _ := do(func(r *http.Request) { r.Header.Del("Authorization") })
	require.Equal(t, http.StatusUnauthorized, status)
	status, _ = do(func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") })
	require.Equal(t, http.StatusUnauthorized, status)
	status, _ = do(func(r *http.Request) { r.Host = "evil.example:80" })
	require.Equal(t, http.StatusForbidden, status)
	status, _ = do(func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") })
	require.Equal(t, http.StatusForbidden, status)
	status, _ = do(func(r *http.Request) { r.Header.Set("Origin", "https://"+r.Host) })
	require.Equal(t, http.StatusForbidden, status)

	status, _ = do(func(r *http.Request) { r.Header.Set("Origin", "null") })
	require.Equal(t, http.StatusForbidden, status)

	// A same-host Origin passes the checks and reaches the handler, and no CORS headers are set.
	status, cors := do(func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) })
	require.NotEqual(t, http.StatusForbidden, status)
	require.Empty(t, cors)
}

func TestSubmitIsUnimplementedOverEveryProtocol(t *testing.T) {
	base, token := startServer(t)

	for name, opts := range map[string][]connect.ClientOption{
		"connect":  nil,
		"grpc":     {connect.WithGRPC()},
		"grpc-web": {connect.WithGRPCWeb()},
	} {
		t.Run(name, func(t *testing.T) {
			client := durablev1connect.NewSessionServiceClient(h2cClient(), base, opts...)

			_, err := client.Submit(t.Context(), connect.NewRequest(&v1.SubmitRequest{}))
			require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))

			req := connect.NewRequest(&v1.SubmitRequest{})
			req.Header().Set("Authorization", "Bearer "+token)
			_, err = client.Submit(t.Context(), req)
			require.Equal(t, connect.CodeUnimplemented, connect.CodeOf(err))
		})
	}
}

func TestGetDoesNotRunDecideApproval(t *testing.T) {
	base, token := startServer(t)
	req, err := http.NewRequest(http.MethodGet, base+durablev1connect.SessionServiceDecideApprovalProcedure+"?connect=v1&encoding=json&message=%7B%7D", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

func TestServesPlaceholderUI(t *testing.T) {
	base, _ := startServer(t)
	// No Authorization header: the UI is exempt from the token.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), "UI not built")
}

func TestServerJSONModeAndCleanup(t *testing.T) {
	r := start(t)
	path := filepath.Join(r.home, ".durable", "server.json")
	st, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())

	require.NoError(t, r.cmd.Process.Signal(syscall.SIGTERM))
	select {
	case <-r.done:
	case <-time.After(15 * time.Second):
		t.Fatal("server did not exit after SIGTERM")
	}
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestShutdownDrainsInFlightRequest(t *testing.T) {
	r := start(t)

	// The body is still open, so the request is in flight when SIGTERM arrives.
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, r.url+durablev1connect.SessionServiceSubmitProcedure, pr)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.token)

	type result struct {
		status int
		err    error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			got <- result{err: err}
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		got <- result{status: resp.StatusCode}
	}()

	_, err = pw.Write([]byte("{"))
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond)
	require.NoError(t, r.cmd.Process.Signal(syscall.SIGTERM))
	time.Sleep(300 * time.Millisecond)
	select {
	case <-r.done:
		t.Fatal("server exited with a request in flight")
	default:
	}

	_, err = pw.Write([]byte("}"))
	require.NoError(t, err)
	require.NoError(t, pw.Close())

	res := <-got
	require.NoError(t, res.err)
	require.Equal(t, http.StatusNotImplemented, res.status)
	select {
	case <-r.done:
	case <-time.After(15 * time.Second):
		t.Fatal("server did not exit after the request finished")
	}
}
