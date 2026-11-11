package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/wolfeidau/durable/internal/creds"
	"github.com/wolfeidau/durable/internal/githost/github"
)

const githubTokenEnv = "DURABLE_GITHUB_TOKEN" //nolint:gosec // a variable name, not a credential

var supportedGitHosts = []string{github.Kind}

// gitHostSources maps a host kind to where its token comes from. The error text never includes a flag value, because a
// token pasted here by mistake must not reach the log.
func gitHostSources(flags []string) (map[string]creds.Source, error) {
	sources := map[string]creds.Source{}
	for _, f := range flags {
		kind, value, ok := strings.Cut(f, "=")
		if !ok || value == "" {
			return nil, errors.New("--git-host-token must be <kind>=<source>, where source is an op:// reference or gh")
		}
		if !slices.Contains(supportedGitHosts, kind) {
			return nil, fmt.Errorf("unsupported git host kind in --git-host-token; supported kinds: %s", strings.Join(supportedGitHosts, ", "))
		}
		if _, dup := sources[kind]; dup {
			return nil, fmt.Errorf("--git-host-token given more than once for %s", kind)
		}
		sources[kind] = creds.Source{Flag: "--git-host-token " + kind, Env: githubTokenEnv, Value: value}
	}
	// A variable alone configures GitHub; GITHUB_TOKEN and GH_TOKEN are never read, because they belong to other tools.
	if _, ok := sources[github.Kind]; !ok {
		if v := strings.TrimSpace(os.Getenv(githubTokenEnv)); v != "" {
			sources[github.Kind] = creds.Source{Flag: "--git-host-token github", Env: githubTokenEnv}
		}
	}
	return sources, nil
}

// checkGitHosts resolves and checks every configured token, so a bad one fails startup instead of the first repo session.
func checkGitHosts(ctx context.Context, logger *slog.Logger, sources map[string]creds.Source, apiURL string) error {
	for kind, src := range sources {
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
