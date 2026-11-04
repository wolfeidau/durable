package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/commands"
)

func TestSessionIndexIsWrittenOnce(t *testing.T) {
	ws, home := newWorkspace(t)
	chatOnce(t, binPath, ws, home, textReply("hi"), "go", 1, "--session", "indexed")

	got, err := commands.ListSessions(home)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "indexed", got[0].Name)
	require.Equal(t, ws, got[0].CWD)
	require.False(t, got[0].CreatedAt.IsZero())

	chatOnce(t, binPath, ws, home, textReply("again"), "go again", 1, "--session", "indexed")
	again, err := commands.ListSessions(home)
	require.NoError(t, err)
	require.Equal(t, got, again)
}
