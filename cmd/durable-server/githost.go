package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/wolfeidau/durable/internal/creds"
	"github.com/wolfeidau/durable/internal/githost/github"
)

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
