package github

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/githost"
	"github.com/wolfeidau/durable/internal/sandbox"
)

func TestParseRepoAcceptsCloneForms(t *testing.T) {
	want := githost.Repo{Host: "github.com", Owner: "octo", Name: "hello.world"}
	for _, raw := range []string{
		"https://github.com/octo/hello.world",
		"https://github.com/Octo/Hello.World.git",
		"git@github.com:octo/hello.world.git",
		"ssh://git@github.com/octo/hello.world.git",
		"github.com/octo/hello.world",
		"  github.com/octo/hello.world\n",
	} {
		got, err := New("").ParseRepo(raw)
		require.NoError(t, err, raw)
		require.Equal(t, want, got, raw)
	}
	require.Equal(t, "github.com/octo/hello.world", want.String())
}

func TestParseRepoRejects(t *testing.T) {
	for _, raw := range []string{
		"",
		"octo/hello",
		"https://gitlab.com/o/n",
		"https://github.com.evil.com/o/n",
		"https://github.com/o",
		"https://github.com/o/n/extra",
		"https://github.com/o/n/",
		"https://github.com/o/n?x=1",
		"https://github.com/o/n#frag",
		"https://user@github.com/o/n",
		"https://user:pw@github.com/o/n",
		"ssh://root@github.com/o/n",
		"ssh://github.com/o/n",
		"http://github.com/o/n",
		"git@gitlab.com:o/n",
		"git@github.com:o/n/extra",
		"https://github.com/o_x/n",
		"https://github.com/o/n%2Fx",
		"https://github.com/o/n n",
		"https://github.com/o/..",
		"https://github.com/o/.",
		"https://github.com:8443/o/n",
	} {
		_, err := New("").ParseRepo(raw)
		require.Error(t, err, raw)
	}
}

func TestCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" || r.Header.Get("Authorization") != "Bearer ghp_good" || r.Header.Get("User-Agent") != "durable" {
			http.Error(w, "bad credentials ghp_echo", http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		_, _ = w.Write([]byte(`{"login":"octo"}`))
	}))
	t.Cleanup(srv.Close)
	h := New(srv.URL + "/")

	info, err := h.Check(t.Context(), "ghp_good")
	require.NoError(t, err)
	require.Equal(t, githost.TokenInfo{Login: "octo", Kind: "classic", Scopes: []string{"repo", "read:org"}}, info)

	_, err = h.Check(t.Context(), "ghp_bad")
	require.EqualError(t, err, "GitHub rejected the token: 401 Unauthorized")
	require.NotContains(t, err.Error(), "ghp_")
}

func TestTokenKind(t *testing.T) {
	for token, want := range map[string]string{
		"github_pat_x": "fine-grained", "ghp_x": "classic", "gho_x": "oauth", "ghs_x": "app", "ghu_x": "user-to-server", "x": "unknown",
	} {
		require.Equal(t, want, tokenKind(token))
	}
	require.Empty(t, splitScopes(""))
}

func TestAccess(t *testing.T) {
	repo, err := New("").ParseRepo("github.com/octo/hello")
	require.NoError(t, err)

	got := New("").Access(repo)

	require.Equal(t, "github", got.Kind)
	require.Equal(t, "durable-github", got.Provider)
	require.Equal(t, "durable-github", got.ProfileID)
	require.Equal(t, "GITHUB_TOKEN", got.CredentialEnv)
	require.Equal(t, sandbox.Endpoint{Host: "github.com", Port: 443}, got.Endpoint)
	require.Equal(t, []string{"/usr/bin/git", "/usr/lib/git-core/git-remote-https"}, got.Binaries)
	require.Equal(t, []sandbox.HTTPRule{
		{Method: "GET", Path: "/octo/hello.git/info/refs*"},
		{Method: "POST", Path: "/octo/hello.git/git-upload-pack"},
		{Method: "POST", Path: "/octo/hello.git/git-receive-pack"},
		{Method: "GET", Path: "/octo/hello/info/refs*"},
		{Method: "POST", Path: "/octo/hello/git-upload-pack"},
		{Method: "POST", Path: "/octo/hello/git-receive-pack"},
	}, got.Rules)
	require.Equal(t, map[string]string{
		"credential.https://github.com.helper": `!f() { echo username=x-access-token; echo "password=$GITHUB_TOKEN"; }; f`,
	}, got.GitConfig)
}
