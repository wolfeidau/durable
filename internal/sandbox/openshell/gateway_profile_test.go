package openshell

import (
	"context"
	"testing"
	"time"

	"github.com/ai-openshell/sdk-go/openshell/v1/types"
	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/githost/github"
)

// The gateway reads some fields back differently, so this checks a matching profile is recognised as matching.
func TestGatewayProfileIsKeptWhenItMatchesAndUpdatedWhenStale(t *testing.T) {
	l := gatewayLauncherWith(t, Config{})
	r, err := github.New("").ParseRepo("github.com/octocat/hello-world")
	require.NoError(t, err)
	g := github.New("").Access(r)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = l.c.Providers().Profiles().Delete(ctx, l.ws, g.ProfileID)
	})
	profile := func() *types.ProviderProfile {
		p, err := l.c.Providers().Profiles().Get(t.Context(), l.ws, g.ProfileID)
		require.NoError(t, err)
		return p
	}

	require.NoError(t, l.ensureProfile(t.Context(), g))
	first := profile()
	require.NoError(t, gatewayLauncherWith(t, Config{}).ensureProfile(t.Context(), g))
	require.Equal(t, first.ResourceVersion, profile().ResourceVersion, "a matching profile is not updated")

	stale := types.ProfileImportItem{Source: "durable", Profile: types.ProviderProfile{
		ID: g.ProfileID, DisplayName: "stale", Category: types.ProfileCategorySourceControl,
		Credentials: []types.ProfileCredential{{Name: g.CredentialEnv, EnvVars: []string{g.CredentialEnv}, Required: true, AuthStyle: "basic"}},
	}}
	_, err = l.c.Providers().Profiles().Update(t.Context(), l.ws, g.ProfileID, first.ResourceVersion, stale)
	require.NoError(t, err)
	require.NoError(t, gatewayLauncherWith(t, Config{}).ensureProfile(t.Context(), g))
	require.Equal(t, "bearer", profile().Credentials[0].AuthStyle, "a stale profile is replaced")
}
