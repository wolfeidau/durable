package main

import (
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/sandbox"
)

func TestInvalidGitAuthorFailsStartup(t *testing.T) {
	for _, v := range []string{"nobody", "<durable@localhost>", ""} {
		out := runFailingIn(t, t.TempDir(), nil, "--git-author", v)

		require.Contains(t, out, "--git-author")
		require.Contains(t, out, "must be NAME <EMAIL>")
	}
}

func TestNewSessionPinsTheGitAuthor(t *testing.T) {
	for name, tc := range map[string]struct {
		args        []string
		name, email string
	}{
		"default": {nil, "durable", "durable@localhost"},
		"flag":    {[]string{"--git-author", "Ada Lovelace <ada@example.com>"}, "Ada Lovelace", "ada@example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			_, home := newWorkspace(t)
			writeEnvironments(t, home, goEnvironments)
			c := newClient(envServer(t, home, tc.args...))

			_, err := c.createEmptyIn(t, "go", "req-author-0123456789")

			// There is no gateway, so the create fails after the pin is written.
			require.Equal(t, connect.CodeUnavailable, connect.CodeOf(err), "%v", err)
			pins, gerr := filepath.Glob(filepath.Join(home, ".durable", "sessions", "*", "sandbox.json"))
			require.NoError(t, gerr)
			require.Len(t, pins, 1)
			pin, ok, err := sandbox.ReadPin(filepath.Dir(pins[0]))
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, map[string]string{"user.name": tc.name, "user.email": tc.email}, pin.GitConfig)
		})
	}
}
