package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/api/durable/v1/durablev1connect"
)

func tokenPath(home string) string { return filepath.Join(home, ".durable", "server-token") }

func runSub(t *testing.T, home string, args ...string) (string, string, error) {
	t.Helper()
	// A serve that fails to refuse would otherwise hang the test until the package timeout.
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Env = append(os.Environ(), "HOME="+home)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func status(t *testing.T, r *running, token string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, r.url+durablev1connect.SessionServiceSubmitProcedure, nil)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestTokenSurvivesRestartAndStaysOutOfOutput(t *testing.T) {
	home := t.TempDir()
	first := startIn(t, home)
	st, err := os.Stat(tokenPath(home))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	first.stop(t)

	second := startIn(t, home)
	require.Equal(t, first.token, second.token)
	second.stop(t)

	require.NotEmpty(t, first.token)
	require.NotContains(t, first.out.String(), first.token)
	require.NotContains(t, second.out.String(), second.token)
	require.Contains(t, second.out.String(), "durable-server url")
}

func TestRotateTokenReplacesToken(t *testing.T) {
	home := t.TempDir()
	first := startIn(t, home)
	first.stop(t)
	oldToken := first.token

	stdout, _, err := runSub(t, home, "rotate-token")
	require.NoError(t, err)
	data, err := os.ReadFile(tokenPath(home))
	require.NoError(t, err)
	newToken := strings.TrimSpace(string(data))
	require.NotEmpty(t, newToken)
	require.NotEqual(t, oldToken, newToken)
	require.NotContains(t, stdout, newToken)
	require.Contains(t, stdout, "durable-server url")
	st, err := os.Stat(tokenPath(home))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())

	second := startIn(t, home)
	require.Equal(t, newToken, second.token)
	require.Equal(t, http.StatusUnauthorized, status(t, second, oldToken))
	require.NotEqual(t, http.StatusUnauthorized, status(t, second, newToken))
}

func TestServeRefusesOpenTokenFile(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable"), 0o700))
	require.NoError(t, os.WriteFile(tokenPath(home), []byte("secret\n"), 0o644))
	require.NoError(t, os.Chmod(tokenPath(home), 0o644))

	_, stderr, err := runSub(t, home, "--addr", "127.0.0.1:0")
	require.Error(t, err)
	require.Contains(t, stderr, "server-token")
	require.Contains(t, stderr, "chmod 600")
	require.NotContains(t, stderr, "secret")
}

func TestServeRejectsEmptyTokenFile(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable"), 0o700))
	require.NoError(t, os.WriteFile(tokenPath(home), []byte(" \n"), 0o600))

	_, stderr, err := runSub(t, home, "--addr", "127.0.0.1:0")
	require.Error(t, err)
	require.Contains(t, stderr, "empty")
}

func TestURLPrintsBrowserLink(t *testing.T) {
	r := start(t)
	stdout, _, err := runSub(t, r.home, "url")
	require.NoError(t, err)
	require.Equal(t, r.url+"/#token="+r.token+"\n", stdout)
}

func TestURLWithoutServer(t *testing.T) {
	home := t.TempDir()
	_, stderr, err := runSub(t, home, "url")
	require.Error(t, err)
	require.Contains(t, stderr, "no server running")
}

func TestURLWithStaleServerInfo(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable"), 0o700))
	// Port 1 is reserved and refuses connections.
	data, err := json.Marshal(serverInfo{Addr: "127.0.0.1:1", Token: "stale-token"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, ".durable", "server.json"), data, 0o600))

	stdout, stderr, err := runSub(t, home, "url")
	require.Error(t, err)
	require.Contains(t, stderr, "no server running")
	require.NotContains(t, stdout+stderr, "stale-token")
}

func TestBrowserURLUsesLoopbackForWildcardAddress(t *testing.T) {
	require.Equal(t, "http://127.0.0.1:7878/#token=t", browserURL("0.0.0.0:7878", "t"))
	require.Equal(t, "http://127.0.0.1:7878/#token=t", browserURL("[::]:7878", "t"))
	require.Equal(t, "http://127.0.0.1:7878/#token=t", browserURL("127.0.0.1:7878", "t"))
}
