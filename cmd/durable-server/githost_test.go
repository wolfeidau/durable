package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	gitTestToken = "github_pat_testsecret0123456789"
	gitTestRef   = "op://vault/item/field"
)

// fakeGitHub answers /user for gitTestToken only, like GitHub does.
func fakeGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" || r.Header.Get("Authorization") != "Bearer "+gitTestToken {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo")
		_, _ = w.Write([]byte(`{"login":"octo"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// gitEnv returns an environment where op and gh are fakes that print gitTestToken, GitHub is srv, and no ambient token is set.
func gitEnv(t *testing.T, srv *httptest.Server) []string {
	t.Helper()
	bin := t.TempDir()
	scripts := map[string]string{
		"op": `[ "$1 $2 $3" = "read -n ` + gitTestRef + `" ] || exit 1; printf '` + gitTestToken + `'`,
		"gh": `[ "$1 $2" = "auth token" ] || exit 1; echo ` + gitTestToken,
	}
	for name, script := range scripts {
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script+"\n"), 0o700))
	}
	return []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"DURABLE_GITHUB_API_URL=" + srv.URL,
		"DURABLE_GITHUB_TOKEN=",
		"GITHUB_TOKEN=ambient-must-not-be-read",
		"GH_TOKEN=ambient-must-not-be-read",
		"XDG_CONFIG_HOME=" + t.TempDir(),
	}
}

// runFailing runs the server expecting it to exit, and returns its output.
func runFailing(t *testing.T, env []string, args ...string) string {
	t.Helper()
	return runFailingIn(t, t.TempDir(), env, args...)
}

func runFailingIn(t *testing.T, home string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), binPath, append([]string{"--addr", "127.0.0.1:0"}, args...)...)
	cmd.Env = append(append(os.Environ(), "HOME="+home), env...)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("server kept running: %s", out)
	}
	require.Error(t, err, string(out))
	return string(out)
}

func TestGitHostTokenSourcesStartTheServer(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  []string
	}{
		{name: "op", args: []string{"--git-host-token", "github=" + gitTestRef}},
		{name: "gh", args: []string{"--git-host-token", "github=gh"}},
		{name: "environment", env: []string{"DURABLE_GITHUB_TOKEN=" + gitTestToken}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := append(gitEnv(t, fakeGitHub(t)), tc.env...)
			r := startEnv(t, t.TempDir(), env, append([]string{"--sandbox-image", "durable-worker:dev"}, tc.args...)...)
			r.stop(t)

			out := r.out.String()
			require.Contains(t, out, "git host ready")
			require.Contains(t, out, "login=octo")
			require.Contains(t, out, "token_kind=fine-grained")
			require.Contains(t, out, "kind=github")
			require.NotContains(t, out, gitTestToken)
			require.NotContains(t, out, "ambient-must-not-be-read")
		})
	}
}

func TestGitHostTokenRejectedByGitHubFailsStartup(t *testing.T) {
	srv := fakeGitHub(t)
	env := append(gitEnv(t, srv), "DURABLE_GITHUB_TOKEN=github_pat_wrong0123456789")

	out := runFailing(t, env, "--sandbox-image", "durable-worker:dev")

	require.Contains(t, out, "401")
	require.NotContains(t, out, "github_pat_")
}

func TestGitHostTokenConfigurationErrors(t *testing.T) {
	env := gitEnv(t, fakeGitHub(t))
	tests := []struct {
		name string
		args []string
		env  []string
		want string
	}{
		{name: "unsupported kind", args: []string{"--sandbox-image", "img", "--git-host-token", "gitlab=gh"}, want: "supported kinds: github"},
		{name: "duplicate kind", args: []string{"--sandbox-image", "img", "--git-host-token", "github=gh", "--git-host-token", "github=" + gitTestRef}, want: "more than once"},
		{name: "no equals sign", args: []string{"--sandbox-image", "img", "--git-host-token", gitTestToken}, want: "<kind>=<source>"},
		{name: "literal source", args: []string{"--sandbox-image", "img", "--git-host-token", "github=" + gitTestToken}, want: "op:// secret reference, gh or env:NAME"},
		{name: "no sandbox image", args: []string{"--git-host-token", "github=gh"}, want: "needs --sandbox-image"},
		{name: "environment without sandbox image", env: []string{"DURABLE_GITHUB_TOKEN=" + gitTestToken}, want: "needs --sandbox-image"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := runFailing(t, append(env, tc.env...), tc.args...)
			require.Contains(t, out, tc.want)
			require.NotContains(t, out, gitTestToken)
		})
	}
}
