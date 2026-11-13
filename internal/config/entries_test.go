package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEntriesCoverEveryFieldWithSourcesAndNoSecretReferences(t *testing.T) {
	t.Setenv("DURABLE_GITHUB_TOKEN", "")
	file := `{
  "provider": {"name": "openrouter", "model": "vendor/model", "api_key": "op://vault/item/field"},
  "git": {"hosts": {"github": {"token": "op://v/i/f"}}},
  "environments": {"go": {"image": "durable-worker-go:dev", "grants": ["go-modules"]}}
}`
	s, err := Load(writeHome(t, map[string]string{"config.json": file}))
	require.NoError(t, err)

	got := map[string]Entry{}
	var fields []string
	for _, e := range s.Entries() {
		require.NotContains(t, e.Value, "op://")
		got[e.Field] = e
		fields = append(fields, e.Field)
	}

	require.Equal(t, []string{
		"listen.addr", "listen.idle_timeout", "listen.open_timeout", "listen.heartbeat",
		"provider.name", "provider.protocol", "provider.model", "provider.api_key",
		"git.author", "git.hosts.github.token", "environments.go",
	}, fields)
	require.Equal(t, Entry{Field: "provider.model", Value: "vendor/model", Source: SourceFile}, got["provider.model"])
	require.Equal(t, Entry{Field: "provider.api_key", Value: "1Password", Source: SourceFile}, got["provider.api_key"])
	require.Equal(t, "1Password", got["git.hosts.github.token"].Value)
	require.Equal(t, Entry{Field: "listen.addr", Value: "127.0.0.1:7878", Source: SourceDefault}, got["listen.addr"])
	require.Equal(t, Entry{Field: "environments.go", Value: "image=durable-worker-go:dev grants=go-modules", Source: SourceFile}, got["environments.go"])
}
