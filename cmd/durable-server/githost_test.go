package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/config"
	"github.com/wolfeidau/durable/internal/creds"
	"github.com/wolfeidau/durable/internal/githost"
	"github.com/wolfeidau/durable/internal/githost/github"
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
	t.Parallel()
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
	t.Parallel()
	srv := fakeGitHub(t)
	env := append(gitEnv(t, srv), "DURABLE_GITHUB_TOKEN=github_pat_wrong0123456789")

	out := runFailing(t, env, "--sandbox-image", "durable-worker:dev")

	require.Contains(t, out, "401")
	require.NotContains(t, out, "github_pat_")
}

func TestGitHostTokenConfigurationErrors(t *testing.T) {
	t.Parallel()
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
		{name: "literal source", args: []string{"--sandbox-image", "img", "--git-host-token", "github=" + gitTestToken}, want: "op:// secret reference, gh, env:NAME or file:PATH"},
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

func TestGitHostAppConfigStartsServerAndValidatesPermissions(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))

	var instBody atomic.Value
	instBody.Store(`{"id":42,"repository_selection":"selected","account":{"login":"octo-org"},"permissions":{"contents":"write","metadata":"read","pull_requests":"write","issues":"write"}}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(bearer, ".")
		if !ok || len(parts) != 3 {
			http.Error(w, "bad jwt", http.StatusUnauthorized)
			return
		}
		sig, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			http.Error(w, "bad sig", http.StatusUnauthorized)
			return
		}
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
			http.Error(w, "bad sig", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/app":
			_, _ = w.Write([]byte(`{"id":99,"slug":"durable-bot"}`))
		case "/app/installations/42":
			_, _ = w.Write([]byte(instBody.Load().(string)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	home := t.TempDir()
	keyFile := filepath.Join(home, "app.pem")
	require.NoError(t, os.WriteFile(keyFile, []byte(keyPEM), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable"), 0o700))
	cfg := `{
	  "git": {"hosts": {"github": {"app": {"client_id": "Iv23liTEST", "installation_id": 42, "private_key": "file:` + keyFile + `"}}}},
	  "environments": {"default": {"image": "durable-worker:dev", "grants": ["github-api"]}}
	}`
	require.NoError(t, os.WriteFile(filepath.Join(home, ".durable", "config.json"), []byte(cfg), 0o600))

	env := []string{"DURABLE_GITHUB_API_URL=" + srv.URL, "DURABLE_GITHUB_TOKEN=", "XDG_CONFIG_HOME=" + t.TempDir()}
	r := startEnv(t, home, env)
	r.stop(t)

	out := r.out.String()
	require.Contains(t, out, "git host ready")
	require.Contains(t, out, "auth=github-app")
	require.Contains(t, out, "app=durable-bot")
	require.Contains(t, out, "installation_id=42")
	require.Contains(t, out, "account=octo-org")
	require.NotContains(t, out, "PRIVATE KEY")

	// Missing contents permission fails startup upfront.
	instBody.Store(`{"id":42,"account":{"login":"octo-org"},"permissions":{"metadata":"read"}}`)
	failOut := runFailingIn(t, home, env)
	require.Contains(t, failOut, "missing contents permission")
	require.NotContains(t, failOut, "PRIVATE KEY")
}

func TestGitAppAccessFallsBackToAnonymousWhenNotInstalled(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	keyFile := filepath.Join(t.TempDir(), "app.pem")
	require.NoError(t, os.WriteFile(keyFile, []byte(keyPEM), 0o600))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	funcs := gitAppAccessFuncs(map[string]config.GitHostAppSource{
		github.Kind: {ClientID: "Iv23liTEST", PrivateKey: creds.Source{Flag: "test", Value: "file:" + keyFile}},
	}, srv.URL)
	access, err := funcs[github.Kind](t.Context(), githost.Repo{Host: "github.com", Owner: "octocat", Name: "hello-world"}, []string{"github-api"})
	require.NoError(t, err)
	require.Empty(t, access.AuthMode)
	require.Empty(t, access.Provider)
	require.Empty(t, access.CredentialEnv)
	require.Equal(t, "https://github.com/octocat/hello-world.git", access.CloneURL)
}
