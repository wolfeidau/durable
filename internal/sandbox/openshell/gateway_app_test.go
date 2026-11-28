package openshell

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/commands"
	"github.com/wolfeidau/durable/internal/creds"
	"github.com/wolfeidau/durable/internal/githost/github"
	"github.com/wolfeidau/durable/internal/sandbox"
)

func cleanupAppProvider(t *testing.T, l *Launcher, providerName string) {
	t.Helper()
	cleanupProviderProfile(t, l, providerName, github.AppProfile)
}

// TestGatewayGitHubAppMintsViaGatewayAndHidesKey exercises the real OpenShell v0.1.10 gateway's
// github_app_installation refresh strategy against a loopback HTTP token endpoint with an ephemeral RSA key.
func TestGatewayGitHubAppMintsViaGatewayAndHidesKey(t *testing.T) {
	if os.Getenv(gatewayEnv) != "1" {
		t.Skipf("set %s=1 to run against the local OpenShell gateway", gatewayEnv)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))

	const (
		clientID       = "Iv23liDurableTest"
		installationID = int64(424242)
		repoID         = int64(987654321)
		mintedToken    = "ghs_durable_gateway_minted_test_token"
	)
	var mints atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/424242/access_tokens" {
			http.NotFound(w, r)
			return
		}
		bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			http.Error(w, "missing bearer", http.StatusUnauthorized)
			return
		}
		parts := strings.Split(bearer, ".")
		if len(parts) != 3 {
			http.Error(w, "bad jwt", http.StatusUnauthorized)
			return
		}
		sig, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			http.Error(w, "bad sig", http.StatusUnauthorized)
			return
		}
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
			http.Error(w, "sig verify failed", http.StatusUnauthorized)
			return
		}
		claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			http.Error(w, "bad claims", http.StatusUnauthorized)
			return
		}
		var claims struct {
			Iss string `json:"iss"`
		}
		if err := json.Unmarshal(claimsBytes, &claims); err != nil || claims.Iss != clientID {
			http.Error(w, "bad iss", http.StatusUnauthorized)
			return
		}
		var req struct {
			RepositoryIDs []int64           `json:"repository_ids"`
			Permissions   map[string]string `json:"permissions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if len(req.RepositoryIDs) != 1 || req.RepositoryIDs[0] != repoID || req.Permissions["contents"] != "write" {
			http.Error(w, "unexpected scope", http.StatusUnprocessableEntity)
			return
		}
		mints.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      mintedToken,
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	t.Cleanup(issuer.Close)

	tokenURL := issuer.URL + "/app/installations/{installation_id}/access_tokens"
	appCfg := Config{
		Apps: map[string]func(context.Context) (AppCredentials, error){
			"github": func(context.Context) (AppCredentials, error) {
				return AppCredentials{ClientID: clientID, PrivateKey: keyPEM}, nil
			},
		},
		AppTokenURL: tokenURL,
	}
	l := gatewayLauncherWith(t, appCfg)

	h := github.New("")
	repo, err := h.ParseRepo("github.com/" + publicRepo)
	require.NoError(t, err)
	access := h.AppAccess(repo, github.AppRepoScope{
		InstallationID: installationID,
		RepositoryID:   repoID,
		Permissions:    map[string]string{"contents": "write", "metadata": "read"},
	})
	cleanupAppProvider(t, l, access.Provider)

	spec := newSandboxWith(t, l, sandbox.Spec{Git: []sandbox.GitAccess{access}})
	require.GreaterOrEqual(t, mints.Load(), int32(1), "gateway must mint an installation token from the issuer")

	// Profile readback must match without bumping ResourceVersion, and a fresh launcher must not reconfigure a healthy provider.
	firstProf, err := l.appRPC.GetProfile(t.Context(), l.ws, github.AppProfile)
	require.NoError(t, err)
	mintsBefore := mints.Load()
	l2 := gatewayLauncherWith(t, appCfg)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	require.NoError(t, l2.Ensure(ctx, spec))
	secondProf, err := l2.appRPC.GetProfile(t.Context(), l2.ws, github.AppProfile)
	require.NoError(t, err)
	require.Equal(t, firstProf.GetResourceVersion(), secondProf.GetResourceVersion(), "matching app profile must not be updated")
	require.Equal(t, mintsBefore, mints.Load(), "fresh launcher must not reconfigure an unchanged healthy app provider")

	// Sandbox sees only the resolver placeholder, never the raw token or private key.
	bash := bashTool(t, dialWorker(t, l, spec.Name, spec.Token))
	out := runBash(t.Context(), t, bash, "ga1", "env | grep '^GITHUB_TOKEN='; echo rc=$?")
	require.Contains(t, out, "GITHUB_TOKEN=openshell:resolve", out)
	require.NotContains(t, out, mintedToken)
	require.NotContains(t, out, "PRIVATE KEY")
}

type liveGitHubAppBox struct {
	repo  string
	scope github.AppRepoScope
	bash  durable.Tool
}

func setupLiveGitHubApp(t *testing.T, optInEnv string, checkApp bool) liveGitHubAppBox {
	t.Helper()
	repo := strings.ToLower(os.Getenv(githubRepoEnv))
	clientID := os.Getenv("DURABLE_TEST_GITHUB_APP_CLIENT_ID")
	keyRef := os.Getenv("DURABLE_TEST_GITHUB_APP_KEY_REF")
	if os.Getenv(optInEnv) != "1" || repo == "" || clientID == "" || keyRef == "" {
		t.Skipf("set %s=1, %s, DURABLE_TEST_GITHUB_APP_CLIENT_ID and DURABLE_TEST_GITHUB_APP_KEY_REF to run live GitHub App tests", optInEnv, githubRepoEnv)
	}
	var instID int64
	if raw := os.Getenv("DURABLE_TEST_GITHUB_APP_INSTALLATION_ID"); raw != "" {
		var err error
		instID, err = strconv.ParseInt(raw, 10, 64)
		require.NoError(t, err)
	}
	keyPEM, err := creds.Resolve(t.Context(), creds.Source{Flag: "DURABLE_TEST_GITHUB_APP_KEY_REF", Value: keyRef})
	require.NoError(t, err)

	h := github.New("")
	appCreds := github.AppCredentials{ClientID: clientID, InstallationID: instID, PrivateKeyPEM: keyPEM}
	if checkApp {
		info, err := h.CheckApp(t.Context(), appCreds, true)
		require.NoError(t, err)
		require.NotEmpty(t, info.Slug)
	}
	parsed, err := h.ParseRepo("github.com/" + repo)
	require.NoError(t, err)
	scope, err := h.ResolveAppRepo(t.Context(), appCreds, parsed, true)
	require.NoError(t, err)
	require.Positive(t, scope.InstallationID)
	require.Positive(t, scope.RepositoryID)
	access := h.AppAccess(parsed, scope)

	l := gatewayLauncherWith(t, Config{
		Apps: map[string]func(context.Context) (AppCredentials, error){
			"github": func(context.Context) (AppCredentials, error) {
				return AppCredentials{ClientID: clientID, PrivateKey: keyPEM}, nil
			},
		},
	})
	cleanupAppProvider(t, l, access.Provider)
	spec := newSandboxWith(t, l, sandbox.Spec{
		Git:    []sandbox.GitAccess{access},
		Grants: commands.BuildGrants([]string{"github-api"}, []sandbox.GitAccess{access}),
	})
	return liveGitHubAppBox{repo: repo, scope: scope, bash: bashTool(t, dialWorker(t, l, spec.Name, spec.Token))}
}

// TestGatewayGitHubAppLiveReadsRepo exercises a real GitHub App installation end-to-end without writing to the repo.
func TestGatewayGitHubAppLiveReadsRepo(t *testing.T) {
	box := setupLiveGitHubApp(t, "DURABLE_TEST_OPENSHELL_GITHUB_APP", true)
	repo, scope := box.repo, box.scope
	run := newBashRunner(t, box.bash, "gr", "PRIVATE KEY", "ghs_")

	out := run("env | grep '^GITHUB_TOKEN='")
	require.Contains(t, out, "GITHUB_TOKEN=openshell:resolve", out)

	out = run("git ls-remote https://github.com/" + repo + ".git HEAD")
	require.Contains(t, out, "rc=0", out)

	out = run("git clone --depth 1 -q https://github.com/" + repo + ".git /sandbox/work && test -f /sandbox/work/README.md")
	require.Contains(t, out, "rc=0", out)

	out = run("git ls-remote https://github.com/" + publicRepo + ".git HEAD")
	require.NotContains(t, out, "rc=0", out)
	require.Contains(t, out, "403", out)

	out = run("gh api repos/" + repo + " --jq .full_name")
	require.Contains(t, out, "rc=0", out)
	require.Contains(t, strings.ToLower(out), repo, out)

	if _, ok := scope.Permissions["pull_requests"]; ok {
		out = run("gh pr list --repo " + repo + " --limit 3")
		require.Contains(t, out, "rc=0", out)
		require.NotContains(t, out, "policy_denied", out)
	}

	out = run("gh api repos/cli/cli")
	require.NotContains(t, out, "rc=0", out)
	require.Contains(t, out, "policy_denied", out)

	out = run("gh api -X PUT repos/" + repo + "/pulls/1/merge")
	require.NotContains(t, out, "rc=0", out)
	require.Contains(t, out, "policy_denied", out)
}

// TestGatewayGitHubAppLivePushesAndQueriesAPI exercises a real write-capable GitHub App installation end-to-end when opted in.
func TestGatewayGitHubAppLivePushesAndQueriesAPI(t *testing.T) {
	box := setupLiveGitHubApp(t, "DURABLE_TEST_OPENSHELL_GITHUB_APP_PUSH", false)
	repo, bash := box.repo, box.bash
	branch := "durable-app-test-" + randHex(t, 4)

	script := strings.Join([]string{
		"cd /sandbox",
		"git clone -q https://github.com/" + repo + ".git work 2>&1",
		"cd work",
		"git -c user.name=durable -c user.email=durable@example.invalid commit --allow-empty -q -m 'durable app test' 2>&1",
		"git push -q origin HEAD:refs/heads/" + branch + " 2>&1",
		"git push -q origin --delete " + branch + " 2>&1",
		"gh pr list --repo " + repo + " 2>&1",
	}, " && ")
	out := runBash(t.Context(), t, bash, "ga_live", script+"; echo rc=$?")
	require.Contains(t, out, "rc=0", out)
	require.NotContains(t, out, "PRIVATE KEY")
}
