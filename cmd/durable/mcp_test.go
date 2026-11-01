package main

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/fakeapi"
)

const fakeServerEnv = "DURABLE_TEST_FAKE_MCP"

func TestMain(m *testing.M) {
	if os.Getenv(fakeServerEnv) == "1" {
		runFakeServer()
		return
	}
	os.Exit(m.Run())
}

// runFakeServer is a stdio MCP server whose tools, delay, marker file, read-only hint and working-directory report file come from the environment.
func runFakeServer() {
	if path := os.Getenv("FAKE_MCP_CWDFILE"); path != "" {
		cwd, _ := os.Getwd()
		_ = os.WriteFile(path, []byte(cwd), 0o600)
	}
	s := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fake", Version: "1"}, nil)
	delay, _ := time.ParseDuration(os.Getenv("FAKE_MCP_SLEEP"))
	for name := range strings.SplitSeq(os.Getenv("FAKE_MCP_TOOLS"), ",") {
		s.AddTool(&mcpsdk.Tool{
			Name:        name,
			Description: "fake tool " + name,
			InputSchema: map[string]any{"type": "object"},
			Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: os.Getenv("FAKE_MCP_READONLY") == "1"},
		}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			if marker := os.Getenv("FAKE_MCP_MARKER"); marker != "" {
				if f, err := os.OpenFile(marker, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
					_, _ = f.WriteString("ran\n")
					_ = f.Close()
				}
			}
			time.Sleep(delay)
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "fake-result"}}}, nil
		})
	}
	_ = s.Run(context.Background(), &mcpsdk.StdioTransport{})
	// A call still sleeping must not keep the process alive once the agent is gone.
	os.Exit(0)
}

func writeMCPConfig(t *testing.T, home string, env map[string]string) {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	all := map[string]string{fakeServerEnv: "1"}
	maps.Copy(all, env)
	writeMCPServers(t, home, map[string]any{"fake": map[string]any{"command": exe, "env": all}})
}

func writeMCPServers(t *testing.T, home string, servers map[string]any) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"mcpServers": servers})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".durable", "mcp.json"), b, 0o600))
}

func mcpCall(name string) string {
	return fakeapi.Stream(apiModel, "tool_use", "null", fakeapi.ToolBlock(0, "toolu_1", name, "{}"))
}

// toolsOf returns the raw tools array of a request body.
func toolsOf(t *testing.T, body string) string {
	t.Helper()
	var req struct {
		Tools json.RawMessage `json:"tools"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	return string(req.Tools)
}

func toolByName(t *testing.T, body, name string) map[string]any {
	t.Helper()
	var list []map[string]any
	require.NoError(t, json.Unmarshal([]byte(toolsOf(t, body)), &list))
	for _, tool := range list {
		if tool["name"] == name {
			return tool
		}
	}
	return nil
}

func waitExit(t *testing.T, a *agent) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- a.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(testTimeout):
		t.Fatalf("agent did not exit: %s", a.out.String())
		return nil
	}
}

func TestMCPToolRoundTrip(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	writeMCPConfig(t, home, map[string]string{"FAKE_MCP_TOOLS": "echo"})
	api := newFakeAPIReply(t, mcpCall("mcp__fake__echo"))

	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	runPrompt(t, api, 2, a, "use it")
	require.Contains(t, a.out.String(), "[mcp] fake: 1 tools")

	reqs := api.requests()
	mcpTool := toolByName(t, reqs[0], "mcp__fake__echo")
	require.NotNil(t, mcpTool, reqs[0])
	require.NotContains(t, mcpTool, "strict")
	require.Equal(t, true, toolByName(t, reqs[0], "bash")["strict"])
	require.True(t, hasToolResult(t, reqs[1], false, "fake-result"), reqs[1])

	hist := finalHistory(t, ws)
	require.Equal(t, []durable.Role{durable.RoleUser, durable.RoleAssistant, durable.RoleTool, durable.RoleAssistant}, historyRoles(hist))
}

func TestMCPReadOnlyHintStillPrompts(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	// kill -9 leaves the server's temp directory behind.
	t.Setenv("TMPDIR", t.TempDir())
	writeMCPConfig(t, home, map[string]string{"FAKE_MCP_TOOLS": "echo", "FAKE_MCP_READONLY": "1"})
	api := newFakeAPIReply(t, mcpCall("mcp__fake__echo"))

	a := startAgent(t, bin, ws, home, api.srv.URL)
	_, err := io.WriteString(a.stdin, "use it\n")
	require.NoError(t, err)
	eventually(t, "approval prompt", func() bool { return strings.Contains(a.out.String(), "[y/N]") })
	a.kill(t)
}

func TestMCPKillDuringCallThenServerRemoved(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	// kill -9 leaves the server's temp directory behind.
	t.Setenv("TMPDIR", t.TempDir())
	marker := filepath.Join(ws, "marker.txt")
	writeMCPConfig(t, home, map[string]string{"FAKE_MCP_TOOLS": "slow", "FAKE_MCP_SLEEP": "5s", "FAKE_MCP_MARKER": marker})
	api := newFakeAPIReply(t, mcpCall("mcp__fake__slow"))

	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	_, err := io.WriteString(a.stdin, "use it\n")
	require.NoError(t, err)
	eventually(t, "marker line", func() bool { return len(readLinesOf(t, marker)) == 1 })
	a.kill(t)

	writeMCPServers(t, home, map[string]any{})
	restart(t, bin, ws, home, api.srv.URL, "--yes")

	require.Len(t, readLinesOf(t, marker), 1, "the MCP call must not run twice")
	reqs := api.requests()
	require.Len(t, reqs, 2)
	require.True(t, hasToolResult(t, reqs[1], true, durable.UnknownOutcome), reqs[1])
	require.Contains(t, toolsOf(t, reqs[0]), "mcp__fake__slow")
	require.Equal(t, toolsOf(t, reqs[0]), toolsOf(t, reqs[1]))

	hist := finalHistory(t, ws)
	require.Equal(t, []durable.Role{durable.RoleUser, durable.RoleAssistant, durable.RoleTool, durable.RoleAssistant}, historyRoles(hist))
}

func TestMCPToolListIsPinnedAcrossRuns(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	writeMCPConfig(t, home, map[string]string{"FAKE_MCP_TOOLS": "alpha,beta"})
	api := newFakeAPIReply(t, textReply("hi"))

	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	runPrompt(t, api, 1, a, "first")

	writeMCPConfig(t, home, map[string]string{"FAKE_MCP_TOOLS": "gamma"})
	b := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	runPrompt(t, api, 2, b, "second")
	require.Contains(t, b.out.String(), "[mcp] using 2 pinned tools; start a new --session to reload MCP servers")

	reqs := api.requests()
	require.Contains(t, toolsOf(t, reqs[0]), "mcp__fake__alpha")
	require.NotContains(t, toolsOf(t, reqs[1]), "gamma")
	require.Equal(t, toolsOf(t, reqs[0]), toolsOf(t, reqs[1]))
}

func TestInvalidMCPConfigFailsWithoutPinning(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".durable", "mcp.json"), []byte("{nope"), 0o600))
	api := newFakeAPIReply(t, textReply("hi"))

	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	require.NoError(t, a.stdin.Close())
	require.Error(t, waitExit(t, a), a.out.String())
	require.Contains(t, a.out.String(), "failed to parse MCP config")

	for _, pin := range []string{"instructions.md", "mcp-tools.json"} {
		pins, err := filepath.Glob(filepath.Join(home, ".durable", "sessions", "*", pin))
		require.NoError(t, err)
		require.Empty(t, pins)
	}
	require.Empty(t, api.requests())
}

func TestMissingMCPServerCommandWarnsAndChatWorks(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	writeMCPServers(t, home, map[string]any{"fake": map[string]any{"command": filepath.Join(home, "no-such-server")}})
	api := newFakeAPIReply(t, textReply("hi"))

	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	runPrompt(t, api, 1, a, "first")
	require.Contains(t, a.out.String(), "[mcp] warning: mcp server \"fake\" skipped")
}

// fakeServers is a config entry for the fake server with extra fields such as useWorkspaceCwd.
func fakeServers(t *testing.T, env map[string]string, extra map[string]any) map[string]any {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	all := map[string]string{fakeServerEnv: "1"}
	maps.Copy(all, env)
	entry := map[string]any{"command": exe, "env": all}
	maps.Copy(entry, extra)
	return map[string]any{"fake": entry}
}

func runCwdTest(t *testing.T, useWorkspaceCwd bool) (ws, got, tmp string) {
	t.Helper()
	tmp = t.TempDir()
	t.Setenv("TMPDIR", tmp)
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	cwdFile := filepath.Join(home, "cwd.txt")
	extra := map[string]any{}
	if useWorkspaceCwd {
		extra["useWorkspaceCwd"] = true
	}
	writeMCPServers(t, home, fakeServers(t, map[string]string{"FAKE_MCP_TOOLS": "echo", "FAKE_MCP_CWDFILE": cwdFile}, extra))
	api := newFakeAPIReply(t, textReply("hi"))

	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	runPrompt(t, api, 1, a, "go")

	b, err := os.ReadFile(cwdFile)
	require.NoError(t, err)
	got, err = filepath.EvalSymlinks(string(b))
	if err != nil {
		// The default directory is already removed once the agent exits.
		got = string(b)
	}
	return ws, got, tmp
}

func TestMCPServerDefaultCwdIsNotWorkspace(t *testing.T) {
	ws, got, tmp := runCwdTest(t, false)
	require.NotEqual(t, ws, got)
	require.Contains(t, got, "durable-mcp-")
	require.Empty(t, mustReadDir(t, tmp), "temp dir must be removed after the agent exits")
}

func TestMCPServerUseWorkspaceCwd(t *testing.T) {
	ws, got, tmp := runCwdTest(t, true)
	require.Equal(t, ws, got)
	require.Empty(t, mustReadDir(t, tmp))
}

func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestInvalidMCPConfigOnResumeWarnsAndCompletesRun(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	marker := filepath.Join(ws, "marker.txt")
	writeMCPConfig(t, home, map[string]string{"FAKE_MCP_TOOLS": "slow", "FAKE_MCP_SLEEP": "5s", "FAKE_MCP_MARKER": marker})
	api := newFakeAPIReply(t, mcpCall("mcp__fake__slow"))

	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	_, err := io.WriteString(a.stdin, "use it\n")
	require.NoError(t, err)
	eventually(t, "marker line", func() bool { return len(readLinesOf(t, marker)) == 1 })
	a.kill(t)

	require.NoError(t, os.WriteFile(filepath.Join(home, ".durable", "mcp.json"), []byte("{nope"), 0o600))
	b := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	require.NoError(t, b.stdin.Close())
	require.NoError(t, waitExit(t, b), b.out.String())
	require.Contains(t, b.out.String(), "[mcp] warning: failed to parse MCP config")

	require.Len(t, readLinesOf(t, marker), 1, "the MCP call must not run twice")
	reqs := api.requests()
	require.Len(t, reqs, 2)
	require.True(t, hasToolResult(t, reqs[1], true, durable.UnknownOutcome), reqs[1])
	hist := finalHistory(t, ws)
	require.Equal(t, []durable.Role{durable.RoleUser, durable.RoleAssistant, durable.RoleTool, durable.RoleAssistant}, historyRoles(hist))
}

func unsetVarConfig(t *testing.T, home string) {
	t.Helper()
	writeMCPServers(t, home, fakeServers(t, map[string]string{"FAKE_MCP_TOOLS": "echo", "TOKEN": "${DURABLE_TEST_UNSET_MCP_VAR}"}, nil))
}

func TestUnsetMCPVariableFailsNewSessionWithoutPinning(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	unsetVarConfig(t, home)
	api := newFakeAPIReply(t, textReply("hi"))

	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	require.NoError(t, a.stdin.Close())
	require.Error(t, waitExit(t, a), a.out.String())
	require.Contains(t, a.out.String(), "DURABLE_TEST_UNSET_MCP_VAR")

	for _, pin := range []string{"instructions.md", "mcp-tools.json"} {
		pins, err := filepath.Glob(filepath.Join(home, ".durable", "sessions", "*", pin))
		require.NoError(t, err)
		require.Empty(t, pins)
	}
	require.Empty(t, api.requests())
}

func TestUnsetMCPVariableOnExistingSessionWarns(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	writeMCPConfig(t, home, map[string]string{"FAKE_MCP_TOOLS": "echo"})
	api := newFakeAPIReply(t, textReply("hi"))
	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	runPrompt(t, api, 1, a, "first")

	unsetVarConfig(t, home)
	b := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	runPrompt(t, api, 2, b, "second")
	require.Contains(t, b.out.String(), "[mcp] warning: ")
	require.Contains(t, b.out.String(), "DURABLE_TEST_UNSET_MCP_VAR")
}
