package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
)

func noop(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	return text("ok"), nil
}

func TestDiscoverMapsTools(t *testing.T) {
	m := newMemServer(nil)
	tool := objectTool("alpha")
	tool.InputSchema = map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}
	tool.Annotations = &mcpsdk.ToolAnnotations{IdempotentHint: true, ReadOnlyHint: true}
	m.server.AddTool(tool, noop)
	m.server.AddTool(objectTool("beta"), noop)
	p := newMemPool(t, map[string]*memServer{"srv": m}, nil)

	pinned, warnings := discover(t, p)
	require.Empty(t, warnings)
	require.Len(t, pinned, 2)
	require.Equal(t, "srv", pinned[0].Server)
	require.Equal(t, "alpha", pinned[0].Tool)
	require.Equal(t, "mcp__srv__alpha", pinned[0].Spec.Name)
	require.Equal(t, "tool alpha", pinned[0].Spec.Description)
	require.True(t, pinned[0].Spec.NonStrict)
	require.JSONEq(t, `{"type":"object","properties":{"q":{"type":"string"}}}`, string(pinned[0].Spec.Schema))

	tools := p.Tools(pinned)
	require.Len(t, tools, 2)
	for i, tl := range tools {
		require.Equal(t, pinned[i].Spec, tl.Spec())
		// Server hints are not trusted.
		require.Equal(t, durable.ReplayNever, tl.Replay())
		_, readOnly := tl.(durable.ReadOnlyTool)
		require.False(t, readOnly)
		_, recoverer := tl.(durable.Recoverer)
		require.False(t, recoverer)
	}
}

func TestToolsDoesNotConnect(t *testing.T) {
	m := newMemServer(nil)
	p := newMemPool(t, map[string]*memServer{"srv": m}, nil)
	p.Tools([]Pinned{{Server: "srv", Tool: "x", Spec: durable.ToolSpec{Name: "mcp__srv__x"}}})
	require.Zero(t, m.dialCount())
}

func TestDiscoverPaginates(t *testing.T) {
	m := newMemServer(&mcpsdk.ServerOptions{PageSize: 2})
	for _, n := range []string{"e", "d", "c", "b", "a"} {
		m.server.AddTool(objectTool(n), noop)
	}
	p := newMemPool(t, map[string]*memServer{"srv": m}, nil)

	pinned, warnings := discover(t, p)
	require.Empty(t, warnings)
	var got []string
	for _, pt := range pinned {
		got = append(got, pt.Tool)
	}
	require.Equal(t, []string{"a", "b", "c", "d", "e"}, got)
}

func TestDiscoverSortsByServerThenTool(t *testing.T) {
	a, b := newMemServer(nil), newMemServer(nil)
	a.server.AddTool(objectTool("z"), noop)
	b.server.AddTool(objectTool("a"), noop)
	p := newMemPool(t, map[string]*memServer{"b": b, "a": a}, nil)

	pinned, _ := discover(t, p)
	require.Equal(t, "mcp__a__z", pinned[0].Spec.Name)
	require.Equal(t, "mcp__b__a", pinned[1].Spec.Name)
}

func TestToolNaming(t *testing.T) {
	require.Equal(t, "mcp__my_server__do_it", toolName("my server", "do.it"))

	long := toolName("srv", strings.Repeat("x", 100))
	require.Len(t, long, 64)
	require.Regexp(t, `^mcp__srv__x+_[0-9a-f]{8}$`, long)
	require.NotEqual(t, long, toolName("srv", strings.Repeat("x", 101)))
}

func TestDiscoverNameCollisionKeepsFirst(t *testing.T) {
	m := newMemServer(nil)
	m.server.AddTool(objectTool("a.b"), noop)
	m.server.AddTool(objectTool("a_b"), noop)
	p := newMemPool(t, map[string]*memServer{"srv": m}, nil)

	pinned, warnings := discover(t, p)
	require.Len(t, pinned, 1)
	require.Equal(t, "a.b", pinned[0].Tool)
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], "a_b")
}

func TestDiscoverSkipsBadSchema(t *testing.T) {
	m := newMemServer(nil)
	m.server.AddTool(objectTool("good"), noop)
	m.server.AddTool(objectTool("bad"), noop)
	m.server.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			res, err := next(ctx, method, req)
			if lt, ok := res.(*mcpsdk.ListToolsResult); ok {
				for _, tl := range lt.Tools {
					if tl.Name == "bad" {
						tl.InputSchema = map[string]any{"type": "string"}
					}
				}
			}
			return res, err
		}
	})
	p := newMemPool(t, map[string]*memServer{"srv": m}, nil)

	pinned, warnings := discover(t, p)
	require.Len(t, pinned, 1)
	require.Equal(t, "good", pinned[0].Tool)
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], "bad")
}

func TestDiscoverCapsDescriptionOnRuneBoundary(t *testing.T) {
	m := newMemServer(nil)
	tl := objectTool("d")
	// 2047 ASCII bytes then a 3-byte rune straddling the 2048 cap.
	tl.Description = strings.Repeat("a", 2047) + "€"
	m.server.AddTool(tl, noop)
	p := newMemPool(t, map[string]*memServer{"srv": m}, nil)

	pinned, _ := discover(t, p)
	require.Equal(t, strings.Repeat("a", 2047), pinned[0].Spec.Description)
}

func TestDiscoverSkipsFailingServer(t *testing.T) {
	m := newMemServer(nil)
	m.server.AddTool(objectTool("ok"), noop)
	p := newMemPool(t, map[string]*memServer{"good": m}, map[string]Server{
		"good": {Type: typeStdio, Command: "x"},
		"bad":  {Type: typeStdio, Command: "x"},
	})
	dial := p.dial
	p.dial = func(name string, s Server) (mcpsdk.Transport, func(), error) {
		if name == "bad" {
			return nil, nil, os.ErrNotExist
		}
		return dial(name, s)
	}

	pinned, warnings := p.Discover(context.Background())
	require.Len(t, pinned, 1)
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], `"bad"`)
}

func TestPinnedRoundTripIsByteIdentical(t *testing.T) {
	m := newMemServer(nil)
	tl := objectTool("r")
	tl.Description = "a <b> & c"
	tl.InputSchema = map[string]any{"type": "object", "description": "x<y&z", "properties": map[string]any{"b": map[string]any{"type": "number"}, "a": map[string]any{"enum": []any{"<", 1.5}}}}
	m.server.AddTool(tl, noop)
	p := newMemPool(t, map[string]*memServer{"srv": m}, nil)

	pinned, _ := discover(t, p)
	first, err := json.Marshal(pinned)
	require.NoError(t, err)
	var back []Pinned
	require.NoError(t, json.Unmarshal(first, &back))
	second, err := json.Marshal(back)
	require.NoError(t, err)
	require.Equal(t, string(first), string(second))
	require.Equal(t, pinned, back)
}

func TestStdioServerEndToEnd(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "leak")
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "leak")
	root := t.TempDir()
	logDir := filepath.Join(t.TempDir(), "logs")
	p := NewPool(map[string]Server{
		"../sneaky": {Type: typeStdio, Command: os.Args[0], Env: map[string]string{stdioServerEnv: "1", "FOO": "bar"}},
	}, Options{Root: root, LogDir: logDir})
	t.Cleanup(func() { _ = p.Close() })

	// Discover cancels its connect context on return; the session must survive that.
	pinned, warnings := p.Discover(context.Background())
	require.Empty(t, warnings)
	require.Len(t, pinned, 1)
	require.Equal(t, "mcp_____sneaky__report", pinned[0].Spec.Name)

	out, err := p.Tools(pinned)[0].Execute(context.Background(), durable.ToolCall{ID: "1"})
	require.NoError(t, err)
	var got map[string]string
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Empty(t, got["anthropic"])
	require.Empty(t, got["op"])
	require.Equal(t, "bar", got["foo"])
	wantCwd, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	gotCwd, err := filepath.EvalSymlinks(got["cwd"])
	require.NoError(t, err)
	require.Equal(t, wantCwd, gotCwd)

	logPath := filepath.Join(logDir, "mcp-___sneaky.log")
	require.Eventually(t, func() bool {
		b, err := os.ReadFile(logPath)
		return err == nil && strings.Contains(string(b), "stdio server started")
	}, 5*time.Second, 20*time.Millisecond)
	info, err := os.Stat(logPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	entries, err := os.ReadDir(logDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}
