package openshell

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/worker"
)

func TestCreateGivesAnEmptySessionTheCommitAuthor(t *testing.T) {
	rec := newRecorder()
	l := newWithClient(rec, "", nil)
	conf := sandbox.GitIdentity{Name: "durable", Email: "durable@localhost"}.Config()

	require.NoError(t, l.Create(t.Context(), sandbox.Spec{Name: "dt-a", Image: "img", Token: "wt", GitConfig: conf}))

	sb, err := rec.Sandboxes().Get(t.Context(), "default", "dt-a")
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		worker.TokenEnv:    "wt",
		"GIT_CONFIG_COUNT": "2",
		"GIT_CONFIG_KEY_0": "user.email", "GIT_CONFIG_VALUE_0": "durable@localhost",
		"GIT_CONFIG_KEY_1": "user.name", "GIT_CONFIG_VALUE_1": "durable",
	}, sb.Spec.Environment)
}

func TestGitEnvMergesSessionConfigWithAccessConfig(t *testing.T) {
	env := gitEnv(
		map[string]string{"user.name": "durable", "user.email": "durable@localhost"},
		[]sandbox.GitAccess{{GitConfig: map[string]string{"credential.helper": "store"}}},
	)

	require.Equal(t, map[string]string{
		"GIT_CONFIG_COUNT": "3",
		"GIT_CONFIG_KEY_0": "credential.helper", "GIT_CONFIG_VALUE_0": "store",
		"GIT_CONFIG_KEY_1": "user.email", "GIT_CONFIG_VALUE_1": "durable@localhost",
		"GIT_CONFIG_KEY_2": "user.name", "GIT_CONFIG_VALUE_2": "durable",
	}, env)
}
