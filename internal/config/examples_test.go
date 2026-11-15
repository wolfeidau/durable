package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExampleConfigsAreValid(t *testing.T) {
	t.Setenv("DURABLE_GITHUB_TOKEN", "")
	paths, err := filepath.Glob(filepath.Join("..", "..", "examples", "config", "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, paths)

	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			body, err := os.ReadFile(p)
			require.NoError(t, err)
			home := writeHome(t, map[string]string{"config.yaml": string(body)})

			s, err := Load(home)

			require.NoError(t, err)
			require.NoError(t, s.Validate())
			require.Empty(t, s.Warnings)
			require.Contains(t, s.GitHosts, "github")
		})
	}
}
