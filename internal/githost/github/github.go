// Package github is the GitHub git host.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/wolfeidau/durable/internal/githost"
	"github.com/wolfeidau/durable/internal/sandbox"
)

const (
	// Kind is the host kind used in flags and the pin.
	Kind = "github"
	// Provider is both the OpenShell provider and profile that hold the token.
	Provider      = "durable-github"
	CredentialEnv = "GITHUB_TOKEN" //nolint:gosec // a variable name, not a credential

	hostname   = "github.com"
	defaultAPI = "https://api.github.com"
)

var (
	ownerRE = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	nameRE  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// Host is GitHub.
type Host struct {
	api    string
	client *http.Client
}

var _ githost.Host = (*Host)(nil)

// New returns GitHub. An empty apiBase means the public API; the override is for tests.
func New(apiBase string) *Host {
	if apiBase == "" {
		apiBase = defaultAPI
	}
	return &Host{api: strings.TrimRight(apiBase, "/"), client: &http.Client{Timeout: 10 * time.Second}}
}

func (*Host) Kind() string { return Kind }

// ParseRepo accepts the usual clone URLs and returns the repository in lower case.
func (*Host) ParseRepo(raw string) (githost.Repo, error) {
	path, ok := repoPath(strings.TrimSpace(raw))
	if !ok {
		return githost.Repo{}, fmt.Errorf("not a %s repository: %q", hostname, raw)
	}
	path = strings.TrimSuffix(path, ".git")
	owner, name, ok := strings.Cut(path, "/")
	if !ok || strings.Contains(name, "/") || !ownerRE.MatchString(owner) || !nameRE.MatchString(name) || name == "." || name == ".." {
		return githost.Repo{}, fmt.Errorf("not a valid %s repository: %q", hostname, raw)
	}
	return githost.Repo{Host: hostname, Owner: strings.ToLower(owner), Name: strings.ToLower(name)}, nil
}

// repoPath returns the owner/name part of raw when it is on GitHub.
func repoPath(raw string) (string, bool) {
	if rest, ok := strings.CutPrefix(raw, "git@"+hostname+":"); ok {
		return rest, !strings.ContainsAny(rest, "?#")
	}
	if !strings.Contains(raw, "://") {
		rest, ok := strings.CutPrefix(raw, hostname+"/")
		return rest, ok && !strings.ContainsAny(rest, "?#")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != hostname || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || strings.Contains(raw, "#") || strings.Contains(raw, "?") {
		return "", false
	}
	switch u.Scheme {
	case "https":
		if u.User != nil {
			return "", false
		}
	case "ssh":
		if _, hasPassword := u.User.Password(); u.User == nil || u.User.Username() != "git" || hasPassword {
			return "", false
		}
	default:
		return "", false
	}
	return strings.TrimPrefix(u.Path, "/"), true
}

// Check returns the token's owner.
func (h *Host) Check(ctx context.Context, token string) (githost.TokenInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.api+"/user", nil)
	if err != nil {
		return githost.TokenInfo{}, fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "durable")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := h.client.Do(req)
	if err != nil {
		return githost.TokenInfo{}, fmt.Errorf("failed to reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return githost.TokenInfo{}, fmt.Errorf("GitHub rejected the token: %s", resp.Status)
	}
	var body struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return githost.TokenInfo{}, fmt.Errorf("failed to decode GitHub response: %w", err)
	}
	return githost.TokenInfo{Login: body.Login, Kind: tokenKind(token), Scopes: splitScopes(resp.Header.Get("X-OAuth-Scopes"))}, nil
}

func tokenKind(token string) string {
	for prefix, kind := range map[string]string{
		"github_pat_": "fine-grained", "ghp_": "classic", "gho_": "oauth", "ghs_": "app", "ghu_": "user-to-server",
	} {
		if strings.HasPrefix(token, prefix) {
			return kind
		}
	}
	return "unknown"
}

func splitScopes(h string) []string {
	var scopes []string
	for s := range strings.SplitSeq(h, ",") {
		if s = strings.TrimSpace(s); s != "" {
			scopes = append(scopes, s)
		}
	}
	return scopes
}

// Access allows the git binaries to fetch and push repo and nothing else on GitHub.
func (*Host) Access(repo githost.Repo) sandbox.GitAccess {
	var rules []sandbox.HTTPRule
	for _, base := range []string{"/" + repo.Owner + "/" + repo.Name + ".git", "/" + repo.Owner + "/" + repo.Name} {
		rules = append(rules,
			sandbox.HTTPRule{Method: "GET", Path: base + "/info/refs*"},
			sandbox.HTTPRule{Method: "POST", Path: base + "/git-upload-pack"},
			sandbox.HTTPRule{Method: "POST", Path: base + "/git-receive-pack"},
		)
	}
	return sandbox.GitAccess{
		Kind:          Kind,
		Provider:      Provider,
		ProfileID:     Provider,
		CredentialEnv: CredentialEnv,
		Endpoint:      sandbox.Endpoint{Host: hostname, Port: 443},
		Rules:         rules,
		Binaries:      []string{"/usr/bin/git", "/usr/lib/git-core/git-remote-https"},
		GitConfig: map[string]string{
			"credential.https://" + hostname + ".helper": `!f() { echo username=x-access-token; echo "password=$` + CredentialEnv + `"; }; f`,
		},
	}
}
