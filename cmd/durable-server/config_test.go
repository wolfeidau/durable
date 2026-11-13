package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
)

func writeConfig(t *testing.T, home, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".durable", "config.json"), []byte(body), 0o600))
}

// runConfig runs durable-server config <args> and returns stdout and stderr together, and the error.
func runConfig(t *testing.T, home string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), binPath, append([]string{"config"}, args...)...)
	cmd.Env = append(os.Environ(), "HOME="+home, "DURABLE_GITHUB_TOKEN=")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInvalidConfigFailsBeforeListeningAndListsEachError(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `{
	  "provider": {"name": "openrouter"},
	  "git": {"author": "nobody", "hosts": {"github": {"token": "gh"}}}
	}`)

	out := runFailingIn(t, home, []string{"DURABLE_GITHUB_TOKEN="})

	require.Contains(t, out, "needs a model")
	require.Contains(t, out, "git.author")
	require.Contains(t, out, "must be NAME <EMAIL>")
	require.Contains(t, out, "needs --sandbox-image or an environment")
	require.NoFileExists(t, filepath.Join(home, ".durable", "server.json"))
	require.NoFileExists(t, filepath.Join(home, ".durable", "server.lock"))
}

func TestSchemaInvalidConfigFailsStartupWithoutEchoingASecret(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `{"provider": {"api_key": "sk-pasted0123456789"}}`)

	out := runFailingIn(t, home, nil)

	require.Contains(t, out, "provider.api_key")
	require.NotContains(t, out, "sk-pasted0123456789")
}

func TestFlagOverridingTheFileWarns(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `{"listen": {"addr": "127.0.0.1:1"}, "provider": {"model": "file/model"}}`)
	r := startIn(t, home, "--model", "flag/model")
	r.stop(t)

	out := r.out.String()
	require.Contains(t, out, "--addr overrides listen.addr")
	require.Contains(t, out, "--model overrides provider.model")
	require.NotContains(t, out, "flag/model")
	require.NotContains(t, out, "file/model")
}

func TestConfigFileEnvironmentsAndLegacyFile(t *testing.T) {
	t.Run("config file", func(t *testing.T) {
		_, home := newWorkspace(t)
		writeConfig(t, home, `{"environments": {"go": {"image": "durable-worker-go:dev", "grants": ["go-modules"]}}}`)
		r := envServer(t, home)
		c := newClient(r)

		resp, err := c.GetServerInfo(t.Context(), authed(c, &v1.GetServerInfoRequest{}))

		require.NoError(t, err)
		require.Len(t, resp.Msg.GetEnvironments(), 1)
		require.Equal(t, "durable-worker-go:dev", resp.Msg.GetEnvironments()[0].GetImage())
		r.stop(t)
		require.NotContains(t, r.out.String(), "deprecated")
	})
	t.Run("legacy file", func(t *testing.T) {
		_, home := newWorkspace(t)
		writeEnvironments(t, home, goEnvironments)
		r := envServer(t, home)
		c := newClient(r)

		resp, err := c.GetServerInfo(t.Context(), authed(c, &v1.GetServerInfoRequest{}))

		require.NoError(t, err)
		require.Len(t, resp.Msg.GetEnvironments(), 1)
		r.stop(t)
		require.Contains(t, r.out.String(), "environments.json is deprecated")
	})
	t.Run("both", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, `{"environments": {"go": {"image": "img"}}}`)
		writeEnvironments(t, home, goEnvironments)

		out := runFailingIn(t, home, nil)

		require.Contains(t, out, "both define environments")
	})
}

func TestConfigCheckPrintsEffectiveSettingsWithSources(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `{
	  "listen": {"idle_timeout": "2m"},
	  "provider": {"name": "openrouter", "model": "vendor/model", "api_key": "op://secret-vault/secret-item/field"},
	  "git": {"hosts": {"github": {"token": "env:MY_GITHUB_TOKEN"}}},
	  "environments": {"go": {"image": "durable-worker-go:dev", "grants": ["go-modules"]}}
	}`)

	out, err := runConfig(t, home, "check")

	require.NoError(t, err, out)
	require.Regexp(t, `listen\.addr\s+127\.0\.0\.1:7878\s+\[default\]`, out)
	require.Regexp(t, `listen\.idle_timeout\s+2m0s\s+\[config\.json\]`, out)
	require.Regexp(t, `provider\.api_key\s+1Password\s+\[config\.json\]`, out)
	require.Regexp(t, `git\.hosts\.github\.token\s+env MY_GITHUB_TOKEN\s+\[config\.json\]`, out)
	require.Regexp(t, `environments\.go\s+image=durable-worker-go:dev grants=go-modules\s+\[config\.json\]`, out)
	require.NotContains(t, out, "secret-vault")
	require.NotContains(t, out, "op://")
}

func TestConfigCheckWarnsAboutTheLegacyFile(t *testing.T) {
	home := t.TempDir()
	writeEnvironments(t, home, goEnvironments)

	out, err := runConfig(t, home, "check")

	require.NoError(t, err, out)
	require.Contains(t, out, "warning: ~/.durable/environments.json is deprecated")
	require.Regexp(t, `environments\.go\s.*\[environments\.json\]`, out)
}

func TestConfigCheckExitsNonZeroOnErrors(t *testing.T) {
	t.Run("schema", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, `{"listen": {"nope": 1}}`)

		out, err := runConfig(t, home, "check")

		require.Error(t, err)
		require.Contains(t, out, "listen")
		require.Contains(t, out, "nope")
	})
	t.Run("semantic", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, `{"provider": {"name": "openrouter"}}`)

		out, err := runConfig(t, home, "check")

		require.Error(t, err)
		require.Contains(t, out, "needs a model")
	})
}

func TestConfigSchemaPrintsTheCommittedFile(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "..", "schema", "durable-server.schema.json"))
	require.NoError(t, err)

	out, err := runConfig(t, t.TempDir(), "schema")

	require.NoError(t, err, out)
	require.Equal(t, string(want), out)
}
