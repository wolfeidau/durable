package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/wolfeidau/durable/internal/githost"
)

// ErrAppNotInstalled reports that the GitHub App is not installed on the target repository.
var ErrAppNotInstalled = errors.New("GitHub App is not installed on")

const (
	// AppProfile is the OpenShell profile ID for GitHub App installation tokens.
	AppProfile = "durable-github-app"
	// AppProviderPrefix starts every scoped GitHub App OpenShell provider name.
	AppProviderPrefix = "durable-gh-"
	// DefaultAppTokenURL is the GitHub App installation token URL template for OpenShell profiles.
	DefaultAppTokenURL = "https://api.github.com/app/installations/{installation_id}/access_tokens" //nolint:gosec // URL template, not a credential
)

// AppCredentials holds the GitHub App identity and RSA PEM private key.
type AppCredentials struct {
	ClientID       string
	InstallationID int64
	PrivateKeyPEM  string
}

// AppInfo describes a verified GitHub App and, when pinned, its installation.
type AppInfo struct {
	Slug                string
	ID                  int64
	InstallationID      int64
	AccountLogin        string
	RepositorySelection string
	Permissions         map[string]string
}

// AppRepoScope is the resolved GitHub App installation scope for one repository.
type AppRepoScope struct {
	InstallationID int64
	RepositoryID   int64
	Permissions    map[string]string
}

type installationResponse struct {
	ID                  int64             `json:"id"`
	RepositorySelection string            `json:"repository_selection"`
	Permissions         map[string]string `json:"permissions"`
	SuspendedAt         *string           `json:"suspended_at"`
	Account             struct {
		Login string `json:"login"`
	} `json:"account"`
}

func (h *Host) appJWT(creds AppCredentials) (string, error) {
	key, err := parsePrivateKey(creds.PrivateKeyPEM)
	if err != nil {
		return "", err
	}
	return signAppJWT(creds.ClientID, key, time.Now())
}

func (h *Host) doJSON(ctx context.Context, method, path, bearer string, body any, out any) (int, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("failed to encode request: %w", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.api+path, r)
	if err != nil {
		return 0, fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("User-Agent", "durable")
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if msg := githubErrorMessage(resp.Body); msg != "" {
			return resp.StatusCode, fmt.Errorf("GitHub returned %s: %s", resp.Status, msg)
		}
		return resp.StatusCode, fmt.Errorf("GitHub returned %s", resp.Status)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("failed to decode GitHub response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

const (
	maxGitHubErrorBody    = 4096
	maxGitHubErrorMessage = 200
)

func githubErrorMessage(body io.Reader) string {
	var e struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(body, maxGitHubErrorBody)).Decode(&e); err != nil {
		return ""
	}
	msg := strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, e.Message))
	if len(msg) > maxGitHubErrorMessage {
		msg = strings.ToValidUTF8(msg[:maxGitHubErrorMessage], "")
	}
	return msg
}

// CheckApp verifies the GitHub App credentials and its installation, failing startup on bad keys, unknown apps, or missing permissions.
func (h *Host) CheckApp(ctx context.Context, creds AppCredentials, withAPI bool) (AppInfo, error) {
	jwt, err := h.appJWT(creds)
	if err != nil {
		return AppInfo{}, err
	}
	var app struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
	}
	if _, err := h.doJSON(ctx, http.MethodGet, "/app", jwt, nil, &app); err != nil {
		return AppInfo{}, fmt.Errorf("GitHub rejected the app credentials: %w", err)
	}
	info := AppInfo{ID: app.ID, Slug: app.Slug, InstallationID: creds.InstallationID}
	if creds.InstallationID > 0 {
		var inst installationResponse
		if _, err := h.doJSON(ctx, http.MethodGet, "/app/installations/"+strconv.FormatInt(creds.InstallationID, 10), jwt, nil, &inst); err != nil {
			return AppInfo{}, fmt.Errorf("GitHub rejected app installation %d: %w", creds.InstallationID, err)
		}
		if inst.SuspendedAt != nil {
			return AppInfo{}, fmt.Errorf("GitHub App installation %d is suspended", creds.InstallationID)
		}
		if _, err := scopedPermissions(inst.Permissions, withAPI); err != nil {
			return AppInfo{}, fmt.Errorf("GitHub App installation %d: %w", creds.InstallationID, err)
		}
		info.AccountLogin = inst.Account.Login
		info.RepositorySelection = inst.RepositorySelection
		info.Permissions = inst.Permissions
		return info, nil
	}
	var insts []installationResponse
	if _, err := h.doJSON(ctx, http.MethodGet, "/app/installations?per_page=1", jwt, nil, &insts); err != nil {
		return AppInfo{}, fmt.Errorf("failed to list GitHub App installations: %w", err)
	}
	if len(insts) == 0 {
		return AppInfo{}, fmt.Errorf("GitHub App %q has no installations", app.Slug)
	}
	return info, nil
}

// ResolveAppRepo resolves the GitHub App installation, numeric repository ID, and scoped token permissions for repo.
func (h *Host) ResolveAppRepo(ctx context.Context, creds AppCredentials, repo githost.Repo, withAPI bool) (AppRepoScope, error) {
	jwt, err := h.appJWT(creds)
	if err != nil {
		return AppRepoScope{}, err
	}
	var inst installationResponse
	status, err := h.doJSON(ctx, http.MethodGet, "/repos/"+repo.Owner+"/"+repo.Name+"/installation", jwt, nil, &inst)
	if err != nil {
		if status == http.StatusNotFound {
			return AppRepoScope{}, fmt.Errorf("%w %s/%s", ErrAppNotInstalled, repo.Owner, repo.Name)
		}
		return AppRepoScope{}, fmt.Errorf("failed to look up GitHub App installation for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	if creds.InstallationID > 0 && inst.ID != creds.InstallationID {
		return AppRepoScope{}, fmt.Errorf("repository %s/%s belongs to GitHub App installation %d, not configured installation %d", repo.Owner, repo.Name, inst.ID, creds.InstallationID)
	}
	if inst.SuspendedAt != nil {
		return AppRepoScope{}, fmt.Errorf("GitHub App installation %d is suspended", inst.ID)
	}
	perms, err := scopedPermissions(inst.Permissions, withAPI)
	if err != nil {
		return AppRepoScope{}, fmt.Errorf("GitHub App installation %d for %s/%s: %w", inst.ID, repo.Owner, repo.Name, err)
	}
	repoID, err := h.resolveRepoID(ctx, jwt, inst.ID, repo)
	if err != nil {
		return AppRepoScope{}, err
	}
	return AppRepoScope{InstallationID: inst.ID, RepositoryID: repoID, Permissions: perms}, nil
}

// resolveRepoID mints a metadata-only token scoped to repo.Name to look up the numeric repository ID for private and public repos alike.
func (h *Host) resolveRepoID(ctx context.Context, jwt string, installationID int64, repo githost.Repo) (int64, error) {
	var minted struct {
		Token        string `json:"token"`
		Repositories []struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	reqBody := map[string]any{
		"repositories": []string{repo.Name},
		"permissions":  map[string]string{"metadata": "read"},
	}
	if _, err := h.doJSON(ctx, http.MethodPost, "/app/installations/"+strconv.FormatInt(installationID, 10)+"/access_tokens", jwt, reqBody, &minted); err != nil {
		return 0, fmt.Errorf("failed to resolve repository ID for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	if minted.Token != "" {
		// Best-effort revocation of the temporary lookup token.
		_, _ = h.doJSON(ctx, http.MethodDelete, "/installation/token", minted.Token, nil, nil)
	}
	want := repo.Owner + "/" + repo.Name
	for _, r := range minted.Repositories {
		if strings.EqualFold(r.FullName, want) && r.ID > 0 {
			return r.ID, nil
		}
	}
	return 0, fmt.Errorf("GitHub App installation %d does not have access to %s", installationID, want)
}
