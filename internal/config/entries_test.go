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
  "environments": {"go": {"image": "durable-worker-go:dev", "grants": ["go-modules"], "approvals": "auto"}}
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
		"provider.name", "provider.protocol", "provider.model", "provider.effort", "provider.api_key",
		"git.author", "git.hosts.github.token", "otel.endpoint", "environments.go",
	}, fields)
	require.Equal(t, Entry{Field: "otel.endpoint", Value: "off", Source: SourceDefault}, got["otel.endpoint"])
	require.Equal(t, Entry{Field: "provider.model", Value: "vendor/model", Source: Source("config.json")}, got["provider.model"])
	require.Equal(t, Entry{Field: "provider.api_key", Value: "1Password", Source: Source("config.json")}, got["provider.api_key"])
	require.Equal(t, "1Password", got["git.hosts.github.token"].Value)
	require.Equal(t, Entry{Field: "listen.addr", Value: "127.0.0.1:7878", Source: SourceDefault}, got["listen.addr"])
	require.Equal(t, Entry{Field: "environments.go", Value: "image=durable-worker-go:dev grants=go-modules approvals=auto", Source: Source("config.json")}, got["environments.go"])
}

func TestOTelEntriesNeverPrintTheReference(t *testing.T) {
	t.Setenv("DURABLE_GITHUB_TOKEN", "")
	file := `{"otel": {"endpoint": "http://localhost:4318", "headers": {"authorization": "op://vault/item/field", "x-team": "env:TEAM"}}}`
	s, err := Load(writeHome(t, map[string]string{"config.json": file}))
	require.NoError(t, err)

	got := map[string]Entry{}
	for _, e := range s.Entries() {
		require.NotContains(t, e.Value, "op://")
		got[e.Field] = e
	}

	require.Equal(t, "http://localhost:4318", got["otel.endpoint"].Value)
	require.Equal(t, Source("config.json"), got["otel.endpoint"].Source)
	require.Equal(t, "http/protobuf", got["otel.protocol"].Value)
	require.Equal(t, "false", got["otel.capture_content"].Value)
	require.Equal(t, "1Password", got["otel.headers.authorization"].Value)
	require.Equal(t, "env TEAM", got["otel.headers.x-team"].Value)
}

func TestFileRefsAreAcceptedAndShownByPath(t *testing.T) {
	t.Setenv("DURABLE_GITHUB_TOKEN", "")
	file := `{
  "provider": {"name": "openrouter", "api_key": "file:/run/secrets/key"},
  "git": {"hosts": {"github": {"token": "file:~/.tok"}}},
  "otel": {"endpoint": "http://localhost:4318", "headers": {"authorization": "file:/run/secrets/otel"}}
}`
	s, err := Load(writeHome(t, map[string]string{"config.json": file}))
	require.NoError(t, err)

	got := map[string]string{}
	for _, e := range s.Entries() {
		got[e.Field] = e.Value
	}
	require.Equal(t, "file /run/secrets/key", got["provider.api_key"])
	require.Equal(t, "file ~/.tok", got["git.hosts.github.token"])
	require.Equal(t, "file /run/secrets/otel", got["otel.headers.authorization"])
}

func TestMalformedFileRefsAreRejected(t *testing.T) {
	for _, bad := range []string{"file:", "file:relative"} {
		for name, body := range map[string]string{
			"provider.api_key":           `{"provider": {"api_key": "` + bad + `"}}`,
			"git.hosts.github.token":     `{"git": {"hosts": {"github": {"token": "` + bad + `"}}}}`,
			"otel.headers.authorization": `{"otel": {"endpoint": "http://localhost:4318", "headers": {"authorization": "` + bad + `"}}}`,
		} {
			_, err := Load(writeHome(t, map[string]string{"config.json": body}))
			require.ErrorContains(t, err, name, bad)
		}
	}
}
