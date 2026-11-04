package server_test

import (
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/server"
)

func TestSessionDir(t *testing.T) {
	dir, err := server.SessionDir("/h", "0123456789abcdef")
	require.NoError(t, err)
	require.Equal(t, filepath.Join("/h", ".durable", "sessions", "0123456789abcdef"), dir)

	for _, id := range []string{"../x", "%2e%2e", "../../etc/passwd", "0123456789ABCDEF", "0123456789abcde", "0123456789abcdef0", "", "0123456789abcde/", "0123456789abcdeg", "0123456789abcde\n"} {
		_, err := server.SessionDir("/h", id)
		require.Error(t, err, id)
		require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), id)
	}
}
