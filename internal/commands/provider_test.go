package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func noCheck(route) error { return nil }

func TestNewRouteDefaults(t *testing.T) {
	r, err := newRoute(routeFlags{})
	require.NoError(t, err)
	require.Equal(t, route{Provider: "anthropic", Protocol: "anthropic-messages"}, r)

	r, err = newRoute(routeFlags{Provider: "anthropic", Model: "claude-opus-5"})
	require.NoError(t, err)
	require.Empty(t, r.Model, "anthropic models are not pinned")

	r, err = newRoute(routeFlags{Provider: "openrouter", Model: "openai/x"})
	require.NoError(t, err)
	require.Equal(t, route{Provider: "openrouter", Protocol: "openai-responses", Model: "openai/x"}, r)
}

func TestNewRouteErrors(t *testing.T) {
	for _, tc := range []struct {
		f    routeFlags
		want string
	}{
		{routeFlags{Provider: "nope"}, "unknown provider"},
		{routeFlags{Provider: "anthropic", Protocol: "openai-responses"}, "anthropic-messages"},
		{routeFlags{Provider: "openrouter", Protocol: "bogus", Model: "m"}, "openai-responses, anthropic-messages"},
		{routeFlags{Provider: "openrouter"}, "--model is required"},
	} {
		_, err := newRoute(tc.f)
		require.ErrorContains(t, err, tc.want)
	}
}

func TestPinRouteValidatesBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	_, _, err := pinRoute(dir, false, routeFlags{Provider: "openrouter"}, noCheck)
	require.Error(t, err)

	checkErr := os.ErrInvalid
	_, _, err = pinRoute(dir, false, routeFlags{Provider: "openrouter", Model: "m"}, func(route) error { return checkErr })
	require.ErrorIs(t, err, checkErr)

	_, statErr := os.Stat(filepath.Join(dir, providerPinFile))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestPinRouteResumesFromPin(t *testing.T) {
	dir := t.TempDir()
	_, pinned, err := pinRoute(dir, false, routeFlags{Provider: "openrouter", Protocol: "anthropic-messages", Model: "anthropic/c"}, noCheck)
	require.NoError(t, err)
	require.False(t, pinned)

	r, pinned, err := pinRoute(dir, true, routeFlags{}, noCheck)
	require.NoError(t, err)
	require.True(t, pinned)
	require.Equal(t, route{Provider: "openrouter", Protocol: "anthropic-messages", Model: "anthropic/c"}, r)

	_, _, err = pinRoute(dir, true, routeFlags{Provider: "anthropic"}, noCheck)
	require.ErrorContains(t, err, "openrouter, not anthropic")
	_, _, err = pinRoute(dir, true, routeFlags{Protocol: "openai-responses"}, noCheck)
	require.ErrorContains(t, err, "anthropic-messages, not openai-responses")
	_, _, err = pinRoute(dir, true, routeFlags{Model: "other"}, noCheck)
	require.ErrorContains(t, err, "anthropic/c, not other")
}

func TestPinRouteOlderSessionIsAnthropic(t *testing.T) {
	dir := t.TempDir()
	r, _, err := pinRoute(dir, true, routeFlags{Model: "claude-opus-5"}, noCheck)
	require.NoError(t, err)
	require.Equal(t, legacyRoute, r)

	_, _, err = pinRoute(t.TempDir(), true, routeFlags{Provider: "openrouter", Model: "m"}, noCheck)
	require.ErrorContains(t, err, "anthropic, not openrouter")
}

func TestPinRouteCorruptPinIsAnError(t *testing.T) {
	for _, content := range []string{"{not json", `{"provider":"bogus","protocol":"x"}`, `{}`} {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, providerPinFile), []byte(content), 0o600))
		_, _, err := pinRoute(dir, true, routeFlags{}, noCheck)
		require.Error(t, err, content)
	}
}

func TestResolveKeyOrder(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "from-env")
	s := providers[providerOpenRouter]
	r := route{Provider: providerOpenRouter}

	k, err := resolveKey("from-flag", r, s)
	require.NoError(t, err)
	require.Equal(t, "from-flag", k)
	k, err = resolveKey("", r, s)
	require.NoError(t, err)
	require.Equal(t, "from-env", k)

	t.Setenv("OPENROUTER_API_KEY", "")
	_, err = resolveKey("", r, s)
	require.ErrorContains(t, err, "OPENROUTER_API_KEY")

	k, err = resolveKey("", route{Provider: providerAnthropic}, providers[providerAnthropic])
	require.NoError(t, err)
	require.Empty(t, k)
}

func TestRouteLine(t *testing.T) {
	line, warn := routeLine(route{Provider: "openrouter", Protocol: "openai-responses", Model: "openai/x"})
	require.Equal(t, "[model] openrouter via openai-responses: openai/x", line)
	require.Empty(t, warn)

	_, warn = routeLine(route{Provider: "openrouter", Protocol: "openai-responses", Model: "anthropic/claude-sonnet-5-5"})
	require.Contains(t, warn, "--protocol anthropic-messages")
	_, warn = routeLine(route{Provider: "openrouter", Protocol: "openai-responses", Model: "~anthropic/claude-sonnet-latest"})
	require.Contains(t, warn, "--protocol anthropic-messages")
	_, warn = routeLine(route{Provider: "openrouter", Protocol: "anthropic-messages", Model: "anthropic/claude-sonnet-5-5"})
	require.Empty(t, warn)
}
