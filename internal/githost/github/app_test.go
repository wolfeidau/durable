package github

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/githost"
	"github.com/wolfeidau/durable/internal/sandbox"
)

func testRSAKey(t *testing.T) (*rsa.PrivateKey, string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pkcs1 := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	der8, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	pkcs8 := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der8}))
	return key, pkcs1, pkcs8
}

func verifyJWT(token string, pub *rsa.PublicKey, wantIss string) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("expected 3 JWT segments")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return err
	}
	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return err
	}
	if claims.Iss != wantIss {
		return fmt.Errorf("iss %q != %q", claims.Iss, wantIss)
	}
	if claims.Iat >= claims.Exp {
		return errors.New("iat must be before exp")
	}
	return nil
}

func TestParsePrivateKeyAndSignAppJWT(t *testing.T) {
	key, pkcs1, pkcs8 := testRSAKey(t)
	for name, tc := range map[string]struct{ raw, iss string }{
		"pkcs1": {pkcs1, "Iv23liPKCS1"},
		"pkcs8": {pkcs8, "Iv23liPKCS8"},
	} {
		t.Run(name, func(t *testing.T) {
			parsed, err := ParsePrivateKey(tc.raw)
			require.NoError(t, err)
			tok, err := SignAppJWT(tc.iss, parsed, time.Now())
			require.NoError(t, err)
			require.NoError(t, verifyJWT(tok, &key.PublicKey, tc.iss))
		})
	}

	_, err := ParsePrivateKey("not-a-pem-secret-marker")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret-marker")

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ecDER, err := x509.MarshalPKCS8PrivateKey(ecKey)
	require.NoError(t, err)
	ecPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}))
	_, err = ParsePrivateKey(ecPEM)
	require.ErrorContains(t, err, "RSA")
}

func TestCheckApp(t *testing.T) {
	key, pkcs1, _ := testRSAKey(t)
	var (
		instStatus atomic.Int32
		appStatus  atomic.Int32
		instBody   atomic.Value
		listBody   atomic.Value
	)
	instStatus.Store(http.StatusOK)
	appStatus.Store(http.StatusOK)
	instBody.Store(`{"id":42,"repository_selection":"selected","account":{"login":"octo-org"},"permissions":{"contents":"write","metadata":"read","pull_requests":"write","issues":"write"}}`)
	listBody.Store(`[{"id":42}]`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || verifyJWT(bearer, &key.PublicKey, "Iv23liTEST") != nil {
			http.Error(w, "bad jwt", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/app":
			w.WriteHeader(int(appStatus.Load()))
			if appStatus.Load() == http.StatusUnauthorized {
				_, _ = w.Write([]byte(`{"message":"'Expiration time' claim ('exp') is too far in the future"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":99,"slug":"durable-bot"}`))
		case "/app/installations/42":
			w.WriteHeader(int(instStatus.Load()))
			_, _ = w.Write([]byte(instBody.Load().(string)))
		case "/app/installations":
			_, _ = w.Write([]byte(listBody.Load().(string)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	h := New(srv.URL)

	info, err := h.CheckApp(t.Context(), AppCredentials{ClientID: "Iv23liTEST", InstallationID: 42, PrivateKeyPEM: pkcs1}, true)
	require.NoError(t, err)
	require.Equal(t, "durable-bot", info.Slug)
	require.Equal(t, int64(99), info.ID)
	require.Equal(t, int64(42), info.InstallationID)
	require.Equal(t, "octo-org", info.AccountLogin)

	// Without installation_id, checks that at least one installation exists.
	info, err = h.CheckApp(t.Context(), AppCredentials{ClientID: "Iv23liTEST", PrivateKeyPEM: pkcs1}, false)
	require.NoError(t, err)
	require.Zero(t, info.InstallationID)

	listBody.Store(`[]`)
	_, err = h.CheckApp(t.Context(), AppCredentials{ClientID: "Iv23liTEST", PrivateKeyPEM: pkcs1}, false)
	require.ErrorContains(t, err, "has no installations")

	// Read-only installation is accepted and capped to read permissions; missing contents fails upfront.
	instBody.Store(`{"id":42,"account":{"login":"octo-org"},"permissions":{"contents":"read","metadata":"read"}}`)
	_, err = h.CheckApp(t.Context(), AppCredentials{ClientID: "Iv23liTEST", InstallationID: 42, PrivateKeyPEM: pkcs1}, true)
	require.NoError(t, err)

	instBody.Store(`{"id":42,"account":{"login":"octo-org"},"permissions":{"metadata":"read"}}`)
	_, err = h.CheckApp(t.Context(), AppCredentials{ClientID: "Iv23liTEST", InstallationID: 42, PrivateKeyPEM: pkcs1}, true)
	require.ErrorContains(t, err, "missing contents permission")

	// Suspended installation fails upfront.
	instBody.Store(`{"id":42,"suspended_at":"2026-01-01T00:00:00Z","permissions":{"contents":"write"}}`)
	_, err = h.CheckApp(t.Context(), AppCredentials{ClientID: "Iv23liTEST", InstallationID: 42, PrivateKeyPEM: pkcs1}, false)
	require.ErrorContains(t, err, "suspended")

	appStatus.Store(http.StatusUnauthorized)
	_, err = h.CheckApp(t.Context(), AppCredentials{ClientID: "Iv23liTEST", PrivateKeyPEM: pkcs1}, false)
	require.ErrorContains(t, err, "401")
	require.ErrorContains(t, err, "too far in the future")
	require.NotContains(t, err.Error(), "PRIVATE KEY")
}

func TestResolveAppRepoAndAppAccess(t *testing.T) {
	key, pkcs1, _ := testRSAKey(t)
	var revoked atomic.Int32
	instBody := `{"id":42,"account":{"login":"octo"},"permissions":{"contents":"write","metadata":"read","pull_requests":"write","issues":"write","checks":"read"}}`
	tokenResp := `{"token":"ghs_temp_meta","repositories":[{"id":123456,"full_name":"Octo/Hello"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/hello/installation":
			bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || verifyJWT(bearer, &key.PublicKey, "Iv23liTEST") != nil {
				http.Error(w, "bad jwt", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(instBody))
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || verifyJWT(bearer, &key.PublicKey, "Iv23liTEST") != nil {
				http.Error(w, "bad jwt", http.StatusUnauthorized)
				return
			}
			var req struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if !slices.Equal(req.Repositories, []string{"hello"}) || req.Permissions["metadata"] != "read" || len(req.Permissions) != 1 {
				http.Error(w, "unexpected scope", http.StatusUnprocessableEntity)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(tokenResp))
		case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
			if r.Header.Get("Authorization") != "Bearer ghs_temp_meta" {
				http.Error(w, "bad token", http.StatusUnauthorized)
				return
			}
			revoked.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	h := New(srv.URL)
	repo, err := h.ParseRepo("github.com/octo/hello")
	require.NoError(t, err)

	// Git-only session gets contents:write + metadata:read.
	gitScope, err := h.ResolveAppRepo(t.Context(), AppCredentials{ClientID: "Iv23liTEST", PrivateKeyPEM: pkcs1}, repo, false)
	require.NoError(t, err)
	require.Equal(t, int64(42), gitScope.InstallationID)
	require.Equal(t, int64(123456), gitScope.RepositoryID)
	require.Equal(t, map[string]string{"contents": "write", "metadata": "read"}, gitScope.Permissions)
	require.Equal(t, int32(1), revoked.Load())

	// With github-api grant, includes pull_requests, issues, and optional checks:read (but not ungranted actions).
	apiScope, err := h.ResolveAppRepo(t.Context(), AppCredentials{ClientID: "Iv23liTEST", InstallationID: 42, PrivateKeyPEM: pkcs1}, repo, true)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"contents": "write", "metadata": "read", "pull_requests": "write", "issues": "write", "checks": "read"}, apiScope.Permissions)

	gitAccess := h.AppAccess(repo, gitScope)
	apiAccess := h.AppAccess(repo, apiScope)
	require.Equal(t, sandbox.GitAuthModeGitHubApp, gitAccess.AuthMode)
	require.Equal(t, AppProfile, gitAccess.ProfileID)
	require.True(t, strings.HasPrefix(gitAccess.Provider, AppProviderPrefix+"123456-"))
	require.NotEqual(t, gitAccess.Provider, apiAccess.Provider, "different permission scopes get distinct providers")
	require.Empty(t, githost.Anonymous(gitAccess).AuthMode)
	require.Zero(t, githost.Anonymous(gitAccess).RepositoryID)

	// Pinned installation mismatch is rejected.
	_, err = h.ResolveAppRepo(t.Context(), AppCredentials{ClientID: "Iv23liTEST", InstallationID: 99, PrivateKeyPEM: pkcs1}, repo, false)
	require.ErrorContains(t, err, "belongs to GitHub App installation 42, not configured installation 99")

	// Missing repo in installation returns ErrAppNotInstalled.
	otherRepo, err := h.ParseRepo("github.com/octo/missing")
	require.NoError(t, err)
	_, err = h.ResolveAppRepo(t.Context(), AppCredentials{ClientID: "Iv23liTEST", PrivateKeyPEM: pkcs1}, otherRepo, false)
	require.ErrorIs(t, err, ErrAppNotInstalled)
	require.ErrorContains(t, err, "octo/missing")
}

func TestGitHubErrorMessageIsCappedAndSanitised(t *testing.T) {
	require.Empty(t, githubErrorMessage(strings.NewReader("not json")))
	require.Equal(t, "Bad credentials", githubErrorMessage(strings.NewReader(`{"message":"Bad\ncredentials\r"}`)))
	long := githubErrorMessage(strings.NewReader(`{"message":"` + strings.Repeat("x", 500) + `"}`))
	require.Len(t, long, maxGitHubErrorMessage)
}
