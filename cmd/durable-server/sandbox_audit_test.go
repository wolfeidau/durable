package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/sandbox"
)

func writeAuditPin(t *testing.T, home, id string) {
	t.Helper()
	dir := filepath.Join(home, ".durable", "sessions", id)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	data, err := json.Marshal(sandbox.Pin{Name: "dt-63539bd5bc24", Image: "worker:test", Token: "unused"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, sandbox.PinFile), data, 0o600))
}

func TestAuditReportsGatewayError(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeAuditPin(t, home, "2222222222222222")

	out, err := sandboxAudit(t, home, "2222222222222222")

	require.Error(t, err, out)
	require.Contains(t, out, "failed to connect to the gateway")
}

func TestAuditRejectsBadFlags(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	const session = "3333333333333333"
	writeAuditPin(t, home, session)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"negative since", []string{"--since=-1h"}, "--since must be positive"},
		{"removed daily flag", []string{"--daily"}, "unknown flag"},
		{"removed raw flag", []string{"--raw"}, "unknown flag"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := sandboxAudit(t, home, session, tc.args...)
			require.Error(t, err, out)
			require.Contains(t, out, tc.want)
		})
	}
}
