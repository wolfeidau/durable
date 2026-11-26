package main

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/wolfeidau/durable/internal/commands"
	"github.com/wolfeidau/durable/internal/config"
	"github.com/wolfeidau/durable/internal/creds"
	"github.com/wolfeidau/durable/internal/githost"
	"github.com/wolfeidau/durable/internal/githost/github"
	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/sandbox/openshell"
)

// checkGitHosts resolves and checks every configured token or app, so bad credentials fail startup instead of the first repo session.
func checkGitHosts(ctx context.Context, logger *slog.Logger, tokens map[string]creds.Source, apps map[string]config.GitHostAppSource, envs map[string]commands.Environment, apiURL string) error {
	for kind, src := range tokens {
		token, err := creds.Resolve(ctx, src)
		if err != nil {
			return fmt.Errorf("failed to resolve %s token: %w", kind, err)
		}
		info, err := github.New(apiURL).Check(ctx, token)
		if err != nil {
			return fmt.Errorf("failed to check %s token: %w", kind, err)
		}
		logger.Info("git host ready", slog.String("kind", kind), slog.String("login", info.Login),
			slog.String("token_kind", info.Kind), slog.Any("scopes", info.Scopes))
	}
	hasGitHubAPI := false
	for _, e := range envs {
		if slices.Contains(e.Grants, "github-api") {
			hasGitHubAPI = true
			break
		}
	}
	for kind, app := range apps {
		keyPEM, err := creds.Resolve(ctx, app.PrivateKey)
		if err != nil {
			return fmt.Errorf("failed to resolve %s app private key: %w", kind, err)
		}
		info, err := github.New(apiURL).CheckApp(ctx, github.AppCredentials{
			ClientID:       app.ClientID,
			InstallationID: app.InstallationID,
			PrivateKeyPEM:  keyPEM,
		}, hasGitHubAPI)
		if err != nil {
			return fmt.Errorf("failed to check %s app: %w", kind, err)
		}
		attrs := []any{slog.String("kind", kind), slog.String("auth", "github-app"), slog.String("app", info.Slug), slog.Int64("app_id", info.ID)}
		if info.InstallationID > 0 {
			attrs = append(attrs, slog.Int64("installation_id", info.InstallationID), slog.String("account", info.AccountLogin), slog.String("repos", info.RepositorySelection))
		}
		logger.Info("git host ready", attrs...)
	}
	return nil
}

// gitTokenFuncs re-resolves on every call, so a rotated token or a refreshed gh login takes effect without a restart.
func gitTokenFuncs(sources map[string]creds.Source) map[string]func(context.Context) (string, error) {
	if len(sources) == 0 {
		return nil
	}
	funcs := map[string]func(context.Context) (string, error){}
	for kind, src := range sources {
		funcs[kind] = func(ctx context.Context) (string, error) { return creds.Resolve(ctx, src) }
	}
	return funcs
}

// gitAppFuncs re-resolves the private key on every call so a rotated GitHub App key takes effect without a restart.
func gitAppFuncs(sources map[string]config.GitHostAppSource) map[string]func(context.Context) (openshell.AppCredentials, error) {
	if len(sources) == 0 {
		return nil
	}
	funcs := map[string]func(context.Context) (openshell.AppCredentials, error){}
	for kind, src := range sources {
		funcs[kind] = func(ctx context.Context) (openshell.AppCredentials, error) {
			keyPEM, err := creds.Resolve(ctx, src.PrivateKey)
			if err != nil {
				return openshell.AppCredentials{}, err
			}
			return openshell.AppCredentials{ClientID: src.ClientID, PrivateKey: keyPEM}, nil
		}
	}
	return funcs
}

// gitAppAccessFuncs resolves the per-repo installation scope and numeric repository ID at repo session creation.
func gitAppAccessFuncs(sources map[string]config.GitHostAppSource, apiURL string) map[string]func(context.Context, githost.Repo, []string) (sandbox.GitAccess, error) {
	if len(sources) == 0 {
		return nil
	}
	host := github.New(apiURL)
	funcs := map[string]func(context.Context, githost.Repo, []string) (sandbox.GitAccess, error){}
	for kind, src := range sources {
		funcs[kind] = func(ctx context.Context, repo githost.Repo, grants []string) (sandbox.GitAccess, error) {
			keyPEM, err := creds.Resolve(ctx, src.PrivateKey)
			if err != nil {
				return sandbox.GitAccess{}, fmt.Errorf("failed to resolve %s app private key: %w", kind, err)
			}
			scope, err := host.ResolveAppRepo(ctx, github.AppCredentials{
				ClientID:       src.ClientID,
				InstallationID: src.InstallationID,
				PrivateKeyPEM:  keyPEM,
			}, repo, slices.Contains(grants, "github-api"))
			if err != nil {
				return sandbox.GitAccess{}, err
			}
			return host.AppAccess(repo, scope), nil
		}
	}
	return funcs
}
