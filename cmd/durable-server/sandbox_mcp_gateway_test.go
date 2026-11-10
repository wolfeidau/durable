package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ai-openshell/sdk-go/openshell/v1/types"
	"github.com/stretchr/testify/require"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
)

// netGatewayEnv adds tests that reach a public MCP server through the sandbox's egress rule.
const netGatewayEnv = "DURABLE_TEST_OPENSHELL_NET"

// mcpTestImage is built by `mise run image:worker-test`.
const mcpTestImage = "durable-worker:mcptest"

func writeMCPConfig(t *testing.T, home, config string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".durable", "mcp.json"), []byte(config), 0o600))
}

func TestGatewayStdioMCPServerRunsInSandboxAndSurvivesRestart(t *testing.T) {
	_, home, l := gatewayHome(t)
	writeMCPConfig(t, home, `{"mcpServers":{"t":{"command":"/usr/local/bin/mcptest-server","useWorkspaceCwd":true,"env":{"MCPTEST_MARKER":"m1"}}}}`)
	api := sequenceAPI(t,
		toolReply("toolu_where1", "mcp__t__where", `{}`),
		textReply("first done"),
		toolReply("toolu_where2", "mcp__t__where", `{}`),
		textReply("second done"),
	)
	r := startEnv(t, home, apiEnv(api), "--api-token", "dummy", "--idle-timeout", "2s", "--sandbox-image", mcpTestImage)
	c := newClient(r)
	got, err := c.createEmpty(t, "", "req-create-mcp-0123456789")
	require.NoError(t, err)
	id := got.GetSession().GetId()
	pin := sessionPin(t, home, id, l)

	stream := openWatch(t, r, id, true)
	submitAs(t, c, id, "where are you")
	approveSeq(t, c, id, 1)
	items := lastRun(t, collectRuns(t, stream, 1))
	out, ok := toolResult(items, "toolu_where1")
	require.True(t, ok)
	require.Contains(t, out, "uid=1500", "the sandbox user, not the host user")
	require.Contains(t, out, "cwd=/sandbox", "useWorkspaceCwd runs in the sandbox workspace")
	require.Contains(t, out, "marker=m1", "the configured env is passed through")
	require.NoError(t, stream.Close())

	require.Eventually(t, func() bool {
		p, err := phaseOf(t, l, pin.Name)
		return err == nil && p == types.SandboxStopped
	}, time.Minute, 500*time.Millisecond)

	stream = openWatch(t, r, id, true)
	_, err = c.Submit(t.Context(), authed(c, &v1.SubmitRequest{SessionId: id, RequestId: "req-2", Text: "again"}))
	require.NoError(t, err)
	approveSeq(t, c, id, 2)
	items = lastRun(t, collectRuns(t, stream, 2))
	out, ok = toolResult(items, "toolu_where2")
	require.True(t, ok)
	require.Contains(t, out, "uid=1500", "the server restarted with the session")
}

// The endpoint is DeepWiki's public MCP server: streamable HTTP, no authentication, stable and free.
func TestGatewayHTTPMCPServerGoesThroughBridgeWithEgressFiltered(t *testing.T) {
	if os.Getenv(netGatewayEnv) != "1" {
		t.Skipf("set %s=1 to reach a public MCP server", netGatewayEnv)
	}
	_, home, l := gatewayHome(t)
	writeMCPConfig(t, home, `{"mcpServers":{"wiki":{"url":"https://mcp.deepwiki.com/mcp"}}}`)
	api := sequenceAPI(t,
		toolReply("toolu_wiki", "mcp__wiki__read_wiki_structure", `{"repoName":"golang/go"}`),
		toolReply("toolu_curl", "bash", bashInput(t, `curl -sS -m 15 -o /dev/null -w "wiki=%{http_code}\n" https://mcp.deepwiki.com/mcp; curl -sS -m 15 -o /dev/null https://example.org; echo rc=$?`)),
		textReply("done"),
	)
	r := startEnv(t, home, apiEnv(api), "--api-token", "dummy", "--sandbox-image", mcpTestImage)
	c := newClient(r)
	got, err := c.createEmpty(t, "", "req-create-net-0123456789")
	require.NoError(t, err)
	id := got.GetSession().GetId()
	sessionPin(t, home, id, l)

	stream := openWatch(t, r, id, true)
	submitAs(t, c, id, "read the wiki, then try the web")
	approveSeq(t, c, id, 1)
	approveSeq(t, c, id, 2)
	items := lastRun(t, collectRuns(t, stream, 1))

	out, ok := toolResult(items, "toolu_wiki")
	require.True(t, ok)
	require.NotEmpty(t, out)
	out, ok = toolResult(items, "toolu_curl")
	require.True(t, ok)
	require.NotContains(t, out, "rc=0", "curl to a host that is not allowed is denied")
	// The rule matches the worker's descendants, so bash reaches the MCP host too; the docs record this.
	require.Contains(t, out, "wiki=")
	require.NotContains(t, out, "wiki=000", "bash reaches the configured MCP host")
}
