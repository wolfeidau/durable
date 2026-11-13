package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/commands"
)

const fullFile = `{
  "$schema": "` + SchemaID + `",
  "listen": {"addr": "127.0.0.1:9000", "idle_timeout": "2m", "open_timeout": "5s", "heartbeat": "1m30s"},
  "provider": {"name": "openrouter", "protocol": "openai-responses", "model": "vendor/model", "api_key": "op://vault/item/field"},
  "git": {"author": "Ada Lovelace <ada@example.com>", "hosts": {"github": {"token": "gh"}}},
  "environments": {
    "default": {"image": "durable-worker:dev"},
    "go": {"image": "durable-worker-go:dev", "grants": ["go-modules"]}
  }
}`

// writeHome returns a home with the given files under .durable.
func writeHome(t *testing.T, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable"), 0o700))
	for name, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(home, ".durable", name), []byte(body), 0o600))
	}
	return home
}

func TestMissingFileMeansDefaults(t *testing.T) {
	t.Setenv("DURABLE_GITHUB_TOKEN", "")

	s, err := Load(t.TempDir())

	require.NoError(t, err)
	require.NoError(t, s.Validate())
	require.Equal(t, "127.0.0.1:7878", s.Addr)
	require.Equal(t, 5*time.Minute, s.IdleTimeout)
	require.Equal(t, 10*time.Second, s.OpenTimeout)
	require.Equal(t, 20*time.Second, s.Heartbeat)
	require.Equal(t, DefaultGitAuthor, s.GitAuthor)
	require.Empty(t, s.GitHosts)
	require.Empty(t, s.Environments)
	require.Empty(t, s.Warnings)
	require.Equal(t, SourceDefault, s.Sources["listen.addr"])
}

func TestGitHubTokenDefaultsToTheEnvironmentVariableOnlyWhenSet(t *testing.T) {
	t.Setenv("DURABLE_GITHUB_TOKEN", "set")
	s, err := Load(t.TempDir())
	require.NoError(t, err)
	require.Equal(t, map[string]string{"github": "env:DURABLE_GITHUB_TOKEN"}, s.GitHosts)
	require.Equal(t, SourceDefault, s.Sources["git.hosts.github.token"])

	home := writeHome(t, map[string]string{"config.json": `{"git": {"hosts": {"github": {"token": "gh"}}}}`})
	s, err = Load(home)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"github": "gh"}, s.GitHosts)
	require.Equal(t, SourceFile, s.Sources["git.hosts.github.token"])
}

func TestFullFile(t *testing.T) {
	t.Setenv("DURABLE_GITHUB_TOKEN", "")
	s, err := Load(writeHome(t, map[string]string{"config.json": fullFile}))

	require.NoError(t, err)
	require.NoError(t, s.Validate())
	require.Equal(t, "127.0.0.1:9000", s.Addr)
	require.Equal(t, 2*time.Minute, s.IdleTimeout)
	require.Equal(t, 5*time.Second, s.OpenTimeout)
	require.Equal(t, 90*time.Second, s.Heartbeat)
	require.Equal(t, "openrouter", s.Provider)
	require.Equal(t, "openai-responses", s.Protocol)
	require.Equal(t, "vendor/model", s.Model)
	require.Equal(t, "op://vault/item/field", s.APIKey)
	require.Equal(t, "Ada Lovelace", s.GitIdentity().Name)
	require.Equal(t, map[string]string{"github": "gh"}, s.GitHosts)
	require.Equal(t, commands.Environment{Name: "go", Image: "durable-worker-go:dev", Grants: []string{"go-modules"}}, s.Environments["go"])
	require.Equal(t, SourceFile, s.Sources["environments.go"])
	require.Empty(t, s.Warnings)
}

func TestMinimalFile(t *testing.T) {
	t.Setenv("DURABLE_GITHUB_TOKEN", "")
	s, err := Load(writeHome(t, map[string]string{"config.json": `{}`}))

	require.NoError(t, err)
	require.NoError(t, s.Validate())
	require.Equal(t, "127.0.0.1:7878", s.Addr)
}

func TestSchemaFailuresNameThePathAndNeverEchoASecret(t *testing.T) {
	const secret = "ghp_sekret0123456789"
	tests := map[string]struct {
		body string
		want string
	}{
		"unknown key":       {`{"listen": {"adr": "x"}}`, "adr"},
		"unknown top key":   {`{"nope": 1}`, "nope"},
		"bad provider":      {`{"provider": {"name": "gpt"}}`, "provider"},
		"bad protocol":      {`{"provider": {"protocol": "grpc"}}`, "protocol"},
		"bad env name":      {`{"environments": {"bad name": {"image": "i"}}}`, "bad name"},
		"unknown grant":     {`{"environments": {"go": {"image": "i", "grants": ["nope"]}}}`, "grants"},
		"missing image":     {`{"environments": {"go": {}}}`, "image"},
		"bad duration":      {`{"listen": {"idle_timeout": "soon"}}`, "idle_timeout"},
		"bad host kind":     {`{"git": {"hosts": {"gitlab": {"token": "gh"}}}}`, "git.hosts: must be one of [github]"},
		"literal api key":   {`{"provider": {"api_key": "` + secret + `"}}`, "provider.api_key"},
		"literal git token": {`{"git": {"hosts": {"github": {"token": "` + secret + `"}}}}`, "git.hosts.github.token"},
		"gh api key":        {`{"provider": {"api_key": "gh"}}`, "provider.api_key"},
		"object token":      {`{"git": {"hosts": {"github": {"token": {"v": "` + secret + `"}}}}}`, "git.hosts.github.token"},
		"string provider":   {`{"provider": "` + secret + `"}`, "provider: wrong type"},
		"string git":        {`{"git": "` + secret + `"}`, "git: wrong type"},
		"string host":       {`{"git": {"hosts": {"github": "` + secret + `"}}}`, "git.hosts.<name>: wrong type"},
		"secret host kind":  {`{"git": {"hosts": {"` + secret + `": {"token": "gh"}}}}`, "git.hosts: must be one of"},
		"secret provider":   {`{"provider": {"name": "` + secret + `"}}`, "provider.name: must be one of"},
		"secret duration":   {`{"listen": {"heartbeat": "` + secret + `"}}`, "listen.heartbeat: does not match"},
		"top-level array":   {`["` + secret + `"]`, "wrong type"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeHome(t, map[string]string{"config.json": tc.body}))

			require.Error(t, err)
			require.Contains(t, err.Error(), "config.json")
			require.Contains(t, err.Error(), tc.want)
			require.NotContains(t, err.Error(), secret)
		})
	}
}

func TestSyntaxErrorIsReported(t *testing.T) {
	_, err := Load(writeHome(t, map[string]string{"config.json": `{`}))

	require.ErrorContains(t, err, "failed to parse ~/.durable/config.json")
}

func TestSemanticErrorsAreJoined(t *testing.T) {
	t.Setenv("DURABLE_GITHUB_TOKEN", "")
	s, err := Load(writeHome(t, map[string]string{"config.json": `{
	  "provider": {"name": "openrouter"},
	  "git": {"author": "nobody", "hosts": {"github": {"token": "gh"}}}
	}`}))
	require.NoError(t, err)

	err = s.Validate()

	require.ErrorContains(t, err, "needs a model")
	require.ErrorContains(t, err, `git.author "nobody" must be NAME <EMAIL>`)
	require.ErrorContains(t, err, "a git host token needs --sandbox-image or an environment")
}

func TestFlagsOverrideTheFileAndWarnWithoutValues(t *testing.T) {
	t.Setenv("DURABLE_GITHUB_TOKEN", "")
	s, err := Load(writeHome(t, map[string]string{"config.json": fullFile}))
	require.NoError(t, err)

	s.Apply(Overrides{Addr: "127.0.0.1:1", Model: "other/model", GitHostFlags: []string{"github=env:FLAG_TOKEN"}, IdleTimeout: time.Second, APIKeyFlag: "--api-token"})

	require.NoError(t, s.Validate())
	require.Equal(t, "127.0.0.1:1", s.Addr)
	require.Equal(t, "other/model", s.Model)
	require.Equal(t, time.Second, s.IdleTimeout)
	require.Empty(t, s.APIKey)
	require.Equal(t, "env:FLAG_TOKEN", s.GitHosts["github"])
	require.Equal(t, SourceFlag, s.Sources["listen.addr"])
	require.Equal(t, SourceFile, s.Sources["listen.open_timeout"])
	fields := map[string]string{}
	for _, w := range s.Warnings {
		fields[w.Field] = w.Message
		require.NotContains(t, w.Message, "127.0.0.1:1")
		require.NotContains(t, w.Message, "other/model")
		require.NotContains(t, w.Message, "FLAG_TOKEN")
	}
	require.Equal(t, "--addr overrides listen.addr from ~/.durable/config.json", fields["listen.addr"])
	require.Contains(t, fields, "provider.model")
	require.Contains(t, fields, "listen.idle_timeout")
	require.Contains(t, fields, "provider.api_key")
	require.Contains(t, fields, "git.hosts.github.token")
	require.Len(t, s.Warnings, 5)
}

func TestProviderFlagDropsTheOtherProvidersSettings(t *testing.T) {
	t.Setenv("DURABLE_GITHUB_TOKEN", "")
	s, err := Load(writeHome(t, map[string]string{"config.json": fullFile}))
	require.NoError(t, err)

	s.Apply(Overrides{Provider: "anthropic"})

	require.NoError(t, s.Validate())
	require.Equal(t, "anthropic", s.Provider)
	require.Empty(t, s.Protocol)
	require.Empty(t, s.Model)
	require.Empty(t, s.APIKey)
	fields := map[string]string{}
	for _, w := range s.Warnings {
		fields[w.Field] = w.Message
	}
	require.Equal(t, "--provider anthropic ignores provider.model from ~/.durable/config.json, which is for another provider", fields["provider.model"])
	require.Contains(t, fields, "provider.protocol")
	require.Contains(t, fields, "provider.api_key")
}

func TestProviderFlagKeepsSettingsForTheSameProvider(t *testing.T) {
	s, err := Load(writeHome(t, map[string]string{"config.json": `{"provider": {"model": "claude-x"}}`}))
	require.NoError(t, err)

	s.Apply(Overrides{Provider: "anthropic"})

	require.NoError(t, s.Validate())
	require.Equal(t, "claude-x", s.Model)
}

func TestFlagOverDefaultDoesNotWarn(t *testing.T) {
	t.Setenv("DURABLE_GITHUB_TOKEN", "")
	s, err := Load(t.TempDir())
	require.NoError(t, err)

	s.Apply(Overrides{Addr: "127.0.0.1:1", Provider: "openrouter", Model: "m"})

	require.NoError(t, s.Validate())
	require.Empty(t, s.Warnings)
}

func TestFlagErrorsAreCollectedWithoutEchoingTokens(t *testing.T) {
	t.Setenv("DURABLE_GITHUB_TOKEN", "")
	const pasted = "github_pat_pasted0123456789"
	s, err := Load(t.TempDir())
	require.NoError(t, err)

	s.Apply(Overrides{Provider: "openrouter", GitAuthor: "nobody", GitHostFlags: []string{pasted, "gitlab=gh", "github=" + pasted}})
	err = s.Validate()

	require.ErrorContains(t, err, "--git-host-token must be <kind>=<source>")
	require.ErrorContains(t, err, "supported kinds: github")
	require.ErrorContains(t, err, "--git-host-token github must be an op:// secret reference, gh or env:NAME")
	require.ErrorContains(t, err, "--git-author")
	require.ErrorContains(t, err, "needs a model")
	require.NotContains(t, err.Error(), pasted)
}

func TestSandboxImageClashesWithADefaultEnvironment(t *testing.T) {
	s, err := Load(writeHome(t, map[string]string{"config.json": fullFile}))
	require.NoError(t, err)

	s.Apply(Overrides{SandboxImage: "img"})

	require.ErrorContains(t, s.Validate(), `--sandbox-image defines the "default" environment, which ~/.durable/config.json also defines`)
}

func TestSandboxImageAddsTheDefaultEnvironment(t *testing.T) {
	s, err := Load(t.TempDir())
	require.NoError(t, err)

	s.Apply(Overrides{SandboxImage: "img"})

	require.NoError(t, s.Validate())
	require.Equal(t, "img", s.Environments["default"].Image)
	require.Equal(t, SourceFlag, s.Sources["environments.default"])
}

func TestLegacyEnvironmentsFileIsUsedWithAWarning(t *testing.T) {
	s, err := Load(writeHome(t, map[string]string{"environments.json": `{"go": {"image": "img", "grants": ["go-modules"]}}`}))

	require.NoError(t, err)
	require.NoError(t, s.Validate())
	require.Equal(t, "img", s.Environments["go"].Image)
	require.Equal(t, SourceLegacy, s.Sources["environments.go"])
	require.Len(t, s.Warnings, 1)
	require.Contains(t, s.Warnings[0].Message, "environments.json is deprecated")
	require.Contains(t, s.Warnings[0].Message, "config.json")

	s2, err := Load(writeHome(t, map[string]string{"environments.json": `{"default": {"image": "img"}}`}))
	require.NoError(t, err)
	s2.Apply(Overrides{SandboxImage: "img"})
	require.ErrorContains(t, s2.Validate(), "which ~/.durable/environments.json also defines")
}

func TestBothEnvironmentSourcesIsAnError(t *testing.T) {
	s, err := Load(writeHome(t, map[string]string{
		"config.json":       `{"environments": {"a": {"image": "i"}}}`,
		"environments.json": `{"b": {"image": "i"}}`,
	}))
	require.NoError(t, err)

	require.ErrorContains(t, s.Validate(), "both define environments")
	require.Empty(t, s.Warnings)
}

func TestInvalidLegacyFileFailsLoad(t *testing.T) {
	_, err := Load(writeHome(t, map[string]string{"environments.json": `{"go": {"image": "img", "grants": ["nope"]}}`}))

	require.ErrorContains(t, err, "unknown grant")
	require.ErrorContains(t, err, "environments.json")
}

func TestUnknownProviderAndProtocolMismatchAreErrors(t *testing.T) {
	s, err := Load(t.TempDir())
	require.NoError(t, err)

	s.Apply(Overrides{Provider: "nope"})
	require.ErrorContains(t, s.Validate(), `unknown provider "nope"`)

	s, err = Load(t.TempDir())
	require.NoError(t, err)
	s.Apply(Overrides{Protocol: "openai-responses"})
	require.ErrorContains(t, s.Validate(), "provider.name and --protocol: ")
	require.ErrorContains(t, s.Validate(), "does not support protocol")
}

func TestGeneratedSchemaMatchesTheCommittedFile(t *testing.T) {
	want, err := SchemaJSON()
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join("..", "..", "schema", "durable-server.schema.json"))

	require.NoError(t, err)
	require.Equal(t, string(want), string(got), "run go generate ./internal/config")
}
