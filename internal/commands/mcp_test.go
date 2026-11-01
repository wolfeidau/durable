package commands

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/mcp"
)

func samplePinned(name string) []mcp.Pinned {
	return []mcp.Pinned{{Server: "s", Tool: name, Spec: durable.ToolSpec{Name: "mcp__s__" + name, NonStrict: true}}}
}

func discoverTools(tools []mcp.Pinned, warnings ...string) func(context.Context) ([]mcp.Pinned, []string) {
	return func(context.Context) ([]mcp.Pinned, []string) { return tools, warnings }
}

func noToolDiscovery(t *testing.T) func(context.Context) ([]mcp.Pinned, []string) {
	t.Helper()
	return func(context.Context) ([]mcp.Pinned, []string) {
		t.Error("discovery must not run")
		return nil, nil
	}
}

func TestPinMCPToolsFresh(t *testing.T) {
	dir := t.TempDir()
	res, err := pinMCPTools(t.Context(), dir, false, discoverTools(samplePinned("a"), "careful"))
	require.NoError(t, err)
	require.Equal(t, samplePinned("a"), res.Tools)
	require.Equal(t, []string{"careful"}, res.Warnings)
	require.False(t, res.Pinned)

	info, err := os.Stat(filepath.Join(dir, mcpPinFile))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the temp file must be removed")
}

func TestPinMCPToolsReusesPinWithoutDiscovery(t *testing.T) {
	dir := t.TempDir()
	_, err := pinMCPTools(t.Context(), dir, false, discoverTools(samplePinned("a")))
	require.NoError(t, err)

	res, err := pinMCPTools(t.Context(), dir, true, noToolDiscovery(t))
	require.NoError(t, err)
	require.Equal(t, samplePinned("a"), res.Tools)
	require.True(t, res.Pinned)
}

func TestPinMCPToolsNoServersPinsEmptyList(t *testing.T) {
	dir := t.TempDir()
	res, err := pinMCPTools(t.Context(), dir, false, discoverTools(nil))
	require.NoError(t, err)
	require.Empty(t, res.Tools)
	b, err := os.ReadFile(filepath.Join(dir, mcpPinFile))
	require.NoError(t, err)
	require.JSONEq(t, "[]", string(b))
}

func TestPinMCPToolsOlderSessionPinsEmpty(t *testing.T) {
	dir := t.TempDir()
	res, err := pinMCPTools(t.Context(), dir, true, noToolDiscovery(t))
	require.NoError(t, err)
	require.Empty(t, res.Tools)
	require.True(t, res.Pinned)

	res, err = pinMCPTools(t.Context(), dir, true, discoverTools(samplePinned("late")))
	require.NoError(t, err)
	require.Empty(t, res.Tools)
}

func TestPinMCPToolsCorruptPinErrors(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, mcpPinFile), []byte("{nope"), 0o600))
	_, err := pinMCPTools(t.Context(), dir, false, noToolDiscovery(t))
	require.ErrorContains(t, err, "failed to parse pinned MCP tools")
}

func TestPinMCPToolsLostRaceUsesWinner(t *testing.T) {
	dir := t.TempDir()
	// The winner pins while this caller is discovering.
	discover := func(context.Context) ([]mcp.Pinned, []string) {
		_, err := pinMCPTools(t.Context(), dir, false, discoverTools(samplePinned("winner")))
		require.NoError(t, err)
		return samplePinned("loser"), nil
	}
	res, err := pinMCPTools(t.Context(), dir, false, discover)
	require.NoError(t, err)
	require.Equal(t, samplePinned("winner"), res.Tools)
	require.True(t, res.Pinned)
}

func TestPinMCPToolsConcurrentCallersAgree(t *testing.T) {
	dir := t.TempDir()
	const n = 16
	got := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			name := string(rune('a' + i))
			res, err := pinMCPTools(t.Context(), dir, false, discoverTools(samplePinned(name)))
			require.NoError(t, err)
			got[i] = res.Tools[0].Tool
		})
	}
	wg.Wait()
	for i := range n {
		require.Equal(t, got[0], got[i])
	}
}
