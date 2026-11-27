package openshell

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"
	"github.com/ai-openshell/sdk-go/openshell/v1/types"

	"github.com/wolfeidau/durable/internal/sandbox"
)

// syncProviders makes sure each provider the sandbox uses has its profile and current credential material. The gateway
// is only told when the material hash differs from the last one this launcher set, so most opens make no update.
func (l *Launcher) syncProviders(ctx context.Context, access []sandbox.GitAccess) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	done := map[string]bool{}
	for _, g := range access {
		if g.Provider == "" || done[g.Provider] {
			continue
		}
		done[g.Provider] = true
		if g.AuthMode == sandbox.GitAuthModeGitHubApp {
			if err := l.syncAppProvider(ctx, g); err != nil {
				return err
			}
			continue
		}
		if err := l.syncStaticProvider(ctx, g); err != nil {
			return err
		}
	}
	return nil
}

func (l *Launcher) syncStaticProvider(ctx context.Context, g sandbox.GitAccess) error {
	tokenFn := l.tokens[g.Kind]
	if tokenFn == nil {
		return fmt.Errorf("no %s token is configured for this server", g.Kind)
	}
	if err := l.ensureProfile(ctx, g); err != nil {
		return err
	}
	token, err := tokenFn(ctx)
	if err != nil {
		return fmt.Errorf("failed to get %s token: %w", g.Kind, err)
	}
	sum := sha256.Sum256([]byte(token))
	if last, ok := l.synced[g.Provider]; ok && last == sum {
		return nil
	}
	_, err = l.c.Providers().Ensure(ctx, l.ws, &types.Provider{
		Name: g.Provider,
		Type: g.ProfileID,
		Spec: types.ProviderSpec{ProfileWorkspace: l.ws, Credentials: map[string]string{g.CredentialEnv: token}},
	})
	if err != nil {
		return fmt.Errorf("failed to set %s provider: %w", g.Kind, err)
	}
	l.synced[g.Provider] = sum
	return nil
}

// forget drops what syncProviders cached for access, so a provider or profile deleted on the gateway is restored on the
// next create instead of failing every create until a restart.
func (l *Launcher) forget(access []sandbox.GitAccess) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, g := range access {
		delete(l.synced, g.Provider)
		delete(l.profiled, g.ProfileID)
	}
}

// ensureProfile imports the credential-only profile for g once. The request rules live in each sandbox's policy, so
// the profile has no endpoints.
func (l *Launcher) ensureProfile(ctx context.Context, g sandbox.GitAccess) error {
	if l.profiled[g.ProfileID] {
		return nil
	}
	item := types.ProfileImportItem{
		Source: "durable",
		Profile: types.ProviderProfile{
			ID:          g.ProfileID,
			DisplayName: "durable " + g.Kind + " token",
			Category:    types.ProfileCategorySourceControl,
			Credentials: []types.ProfileCredential{{
				Name: g.CredentialEnv, EnvVars: []string{g.CredentialEnv}, Required: true, Secret: true,
				AuthStyle: "bearer", HeaderName: "authorization",
			}},
		},
	}
	existing, err := l.c.Providers().Profiles().Get(ctx, l.ws, g.ProfileID)
	switch {
	case err == nil && sameCredentials(existing.Credentials, item.Profile.Credentials):
	case err == nil:
		// An older or hand-made profile would silently keep its own credential definition.
		if _, err := l.c.Providers().Profiles().Update(ctx, l.ws, g.ProfileID, existing.ResourceVersion, item); err != nil {
			return fmt.Errorf("failed to update %s profile: %w", g.Kind, err)
		}
	case v1.IsNotFound(err):
		if _, err := l.c.Providers().Profiles().Import(ctx, l.ws, []types.ProfileImportItem{item}); err != nil {
			return fmt.Errorf("failed to import %s profile: %w", g.Kind, err)
		}
	default:
		return fmt.Errorf("failed to get %s profile: %w", g.Kind, err)
	}
	l.profiled[g.ProfileID] = true
	return nil
}

// sameCredentials compares the fields durable sets, except Secret, which the gateway (v0.1.7) reads back as false.
func sameCredentials(got, want []types.ProfileCredential) bool {
	return slices.EqualFunc(got, want, func(a, b types.ProfileCredential) bool {
		return a.Name == b.Name && slices.Equal(a.EnvVars, b.EnvVars) && a.Required == b.Required &&
			a.AuthStyle == b.AuthStyle && a.HeaderName == b.HeaderName
	})
}
