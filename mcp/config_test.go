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

func TestLoadConfigYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.yaml")
	require.NoError(t, os.WriteFile(path, []byte("mcpServers:\n  a:\n    command: srv\n    args: [-x]\n  b:\n    url: http://localhost/mcp\n"), 0o600))
	servers, warnings, err := LoadConfig(path)
	require.NoError(t, err)
	require.Empty(t, warnings)
	require.Equal(t, typeStdio, servers["a"].Type)
	require.Equal(t, []string{"-x"}, servers["a"].Args)
	require.Equal(t, typeHTTP, servers["b"].Type)
}

func TestLoadConfigYAMLUnquotedScalars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.yaml")
	body := "mcpServers:\n  a:\n    command: srv\n    args: [--port, 8080, 1.0, true]\n    env: {PORT: 8080, DEBUG: true, V: 1.0}\n  b:\n    url: http://localhost/mcp\n    headers: {X-Id: 42}\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	servers, _, err := LoadConfig(path)
	require.NoError(t, err)
	require.Equal(t, []string{"--port", "8080", "1.0", "true"}, servers["a"].Args)
	require.Equal(t, map[string]string{"PORT": "8080", "DEBUG": "true", "V": "1.0"}, servers["a"].Env)
	require.Equal(t, map[string]string{"X-Id": "42"}, servers["b"].Headers)
}

func TestLoadConfigRejectsNonScalarValues(t *testing.T) {
	for body, want := range map[string]string{
		`{"mcpServers":{"a":{"command":"c","env":{"A":[1]}}}}`:     "env.A must be a string, number or boolean",
		`{"mcpServers":{"a":{"command":"c","env":{"A":null}}}}`:    "env.A must be a string, number or boolean",
		`{"mcpServers":{"a":{"command":"c","args":["x",{}]}}}`:     "args[1] must be a string, number or boolean",
		`{"mcpServers":{"a":{"command":"c","args":"x"}}}`:          "args must be a list",
		`{"mcpServers":{"a":{"url":"u","headers":{"A":{"b":1}}}}}`: "headers.A must be a string, number or boolean",
	} {
		_, _, err := LoadConfig(writeConfig(t, body))
		require.ErrorContains(t, err, want, body)
	}
}
