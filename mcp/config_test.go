package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestLoadConfigMissingFile(t *testing.T) {
	servers, warnings, err := LoadConfig(filepath.Join(t.TempDir(), "absent.json"))
	require.NoError(t, err)
	require.Empty(t, servers)
	require.Empty(t, warnings)
}

func TestLoadConfigInvalidJSON(t *testing.T) {
	_, _, err := LoadConfig(writeConfig(t, `{"mcpServers":`))
	require.Error(t, err)
}

func TestLoadConfigInfersTypeAndIgnoresUnknownFields(t *testing.T) {
	servers, warnings, err := LoadConfig(writeConfig(t, `{"other":1,"mcpServers":{
		"a":{"command":"srv","args":["-x"],"timeoutSeconds":5,"alwaysAllow":["t"]},
		"b":{"url":"http://localhost/mcp"},
		"c":{"type":"http","url":"http://localhost/c"}}}`))
	require.NoError(t, err)
	require.Empty(t, warnings)
	require.Equal(t, typeStdio, servers["a"].Type)
	require.Equal(t, []string{"-x"}, servers["a"].Args)
	require.Equal(t, 5, servers["a"].TimeoutSeconds)
	require.Equal(t, typeHTTP, servers["b"].Type)
	require.Equal(t, typeHTTP, servers["c"].Type)
}

func TestLoadConfigSkipsWithWarning(t *testing.T) {
	servers, warnings, err := LoadConfig(writeConfig(t, `{"mcpServers":{
		"legacy":{"type":"sse","url":"http://localhost/sse"},
		"empty":{},
		"ok":{"command":"srv"}}}`))
	require.NoError(t, err)
	require.Len(t, servers, 1)
	require.Contains(t, servers, "ok")
	require.Len(t, warnings, 2)
	require.Contains(t, warnings[0], "empty")
	require.Contains(t, warnings[1], "legacy")
}

func TestLoadConfigExpandsEnvAndHeaders(t *testing.T) {
	t.Setenv("DURABLE_TEST_TOKEN", "s3cret")
	servers, _, err := LoadConfig(writeConfig(t, `{"mcpServers":{
		"a":{"command":"srv","env":{"TOKEN":"${DURABLE_TEST_TOKEN}","PLAIN":"x"}},
		"b":{"url":"http://h/mcp","headers":{"Authorization":"Bearer ${DURABLE_TEST_TOKEN}"}}}}`))
	require.NoError(t, err)
	require.Equal(t, "s3cret", servers["a"].Env["TOKEN"])
	require.Equal(t, "x", servers["a"].Env["PLAIN"])
	require.Equal(t, "Bearer s3cret", servers["b"].Headers["Authorization"])
}

func TestLoadConfigUnsetVariableSkipsServerAndKeepsOthers(t *testing.T) {
	t.Setenv("DURABLE_TEST_SET_VAR", "s3cret")
	servers, warnings, err := LoadConfig(writeConfig(t, `{"mcpServers":{
		"a":{"command":"srv","env":{"TOKEN":"${DURABLE_TEST_UNSET_VAR}"}},
		"b":{"url":"http://h/mcp","headers":{"Authorization":"${DURABLE_TEST_UNSET_VAR2}"}},
		"c":{"command":"srv","env":{"TOKEN":"${DURABLE_TEST_SET_VAR}"}}}}`))
	require.Error(t, err)
	require.Empty(t, warnings)
	require.Len(t, servers, 1)
	require.Equal(t, "s3cret", servers["c"].Env["TOKEN"])
	for _, want := range []string{`"a"`, `"TOKEN"`, "DURABLE_TEST_UNSET_VAR", `"b"`, `"Authorization"`, "DURABLE_TEST_UNSET_VAR2"} {
		require.Contains(t, err.Error(), want)
	}
	require.NotContains(t, err.Error(), "s3cret")
}

func TestLoadConfigInvalidJSONReturnsNoServers(t *testing.T) {
	servers, _, err := LoadConfig(writeConfig(t, `{"mcpServers":`))
	require.Error(t, err)
	require.Empty(t, servers)
}

func TestLoadConfigUseWorkspaceCwd(t *testing.T) {
	servers, _, err := LoadConfig(writeConfig(t, `{"mcpServers":{"a":{"command":"srv"},"b":{"command":"srv","useWorkspaceCwd":true}}}`))
	require.NoError(t, err)
	require.False(t, servers["a"].UseWorkspaceCwd)
	require.True(t, servers["b"].UseWorkspaceCwd)
}

func TestLoadConfigDoesNotExpandCommandOrArgs(t *testing.T) {
	servers, _, err := LoadConfig(writeConfig(t, `{"mcpServers":{"a":{"command":"${NOPE}","args":["${NOPE}"]}}}`))
	require.NoError(t, err)
	require.Equal(t, "${NOPE}", servers["a"].Command)
	require.Equal(t, []string{"${NOPE}"}, servers["a"].Args)
}
