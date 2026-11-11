package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/sandbox/sandboxtest"
	"github.com/wolfeidau/durable/mcp"
	"github.com/wolfeidau/durable/tools"
)

func sandboxCfg(t *testing.T, f *sandboxtest.Fake, image string) AssembleConfig {
	t.Helper()
	return AssembleConfig{
		CWD: t.TempDir(), Session: "s", APIToken: "k",
		Sandbox: &SandboxConfig{Backend: f, Image: image},
	}
}

// markExisting makes the session look opened before, as durable.Open would.
func markExisting(t *testing.T, cfg AssembleConfig) string {
	t.Helper()
	path, err := SessionPath(cfg.CWD, cfg.Session)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	return filepath.Dir(path)
}

func TestAssembleNewSessionWithImageRunsToolsInSandbox(t *testing.T) {
	assembleHome(t)
	f := sandboxtest.New(t)
	cfg := sandboxCfg(t, f, "img:1")

	a, err := Assemble(t.Context(), cfg)
	require.NoError(t, err)

	pin, ok, err := sandbox.ReadPin(filepath.Dir(a.Path))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "img:1", pin.Image)
	require.Equal(t, []sandbox.Spec{pin.Spec()}, f.Specs())

	var names []string
	for _, e := range tools.Catalog() {
		names = append(names, e.Spec.Name)
	}
	require.Len(t, a.toolset, len(names))
	for i, tl := range a.toolset {
		require.Equal(t, names[i], tl.Spec().Name)
		require.Contains(t, fmt.Sprintf("%T", tl), "remoteTool")
	}

	a.Close()
	_, stops, _ := f.Counts()
	require.Equal(t, 1, stops)
	a.Close()
	_, stops, _ = f.Counts()
	require.Equal(t, 1, stops, "Close is idempotent")
}

func TestAssembleReopenWithoutImageStillUsesSandbox(t *testing.T) {
	assembleHome(t)
	f := sandboxtest.New(t)
	cfg := sandboxCfg(t, f, "img:1")
	a, err := Assemble(t.Context(), cfg)
	require.NoError(t, err)
	a.Close()
	markExisting(t, cfg)

	cfg.Sandbox.Image = ""
	a, err = Assemble(t.Context(), cfg)
	require.NoError(t, err)
	defer a.Close()

	ensures, _, _ := f.Counts()
	require.Equal(t, 2, ensures)
	specs := f.Specs()
	require.Equal(t, specs[0], specs[1])
	require.Contains(t, fmt.Sprintf("%T", a.toolset[0]), "remoteTool")
}

func TestAssembleSandboxedSessionWithoutSupportIsRefused(t *testing.T) {
	assembleHome(t)
	f := sandboxtest.New(t)
	cfg := sandboxCfg(t, f, "img:1")
	a, err := Assemble(t.Context(), cfg)
	require.NoError(t, err)
	a.Close()
	markExisting(t, cfg)

	cfg.Sandbox = nil
	_, err = Assemble(t.Context(), cfg)
	require.ErrorIs(t, err, ErrSandboxed)
}

func TestAssembleSessionWithoutPinStaysLocal(t *testing.T) {
	assembleHome(t)
	f := sandboxtest.New(t)
	cfg := sandboxCfg(t, f, "")
	a, err := Assemble(t.Context(), cfg)
	require.NoError(t, err)
	a.Close()
	dir := markExisting(t, cfg)

	cfg.Sandbox.Image = "img:1"
	a, err = Assemble(t.Context(), cfg)
	require.NoError(t, err)
	defer a.Close()

	ensures, _, _ := f.Counts()
	require.Zero(t, ensures)
	_, err = os.Stat(filepath.Join(dir, sandbox.PinFile))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NotContains(t, fmt.Sprintf("%T", a.toolset[0]), "remoteTool")
}

func TestAssembleFailedOpenKeepsPinAndRetryReusesToken(t *testing.T) {
	assembleHome(t)
	f := sandboxtest.New(t)
	cfg := sandboxCfg(t, f, "img:1")
	f.FailConnect(errors.New("boom"))

	_, err := Assemble(t.Context(), cfg)
	var se *sandbox.Error
	require.ErrorAs(t, err, &se)
	path, err := SessionPath(cfg.CWD, cfg.Session)
	require.NoError(t, err)
	first, ok, err := sandbox.ReadPin(filepath.Dir(path))
	require.NoError(t, err)
	require.True(t, ok)

	f.FailConnect(nil)
	a, err := Assemble(t.Context(), cfg)
	require.NoError(t, err)
	defer a.Close()

	specs := f.Specs()
	require.Len(t, specs, 2)
	require.Equal(t, first.Spec(), specs[1])
}

func TestAssembleLaterFailureDeletesANewSessionsSandboxAndStopsAnExistingOne(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			assembleHome(t)
			f := sandboxtest.New(t)
			cfg := sandboxCfg(t, f, "img:1")
			path, err := SessionPath(cfg.CWD, cfg.Session)
			require.NoError(t, err)
			dir := filepath.Dir(path)
			if existing {
				a, err := Assemble(t.Context(), cfg)
				require.NoError(t, err)
				a.Close()
				markExisting(t, cfg)
				require.NoError(t, os.Remove(filepath.Join(dir, pinFile)))
			}
			_, stops0, _ := f.Counts()
			// An unreadable instructions pin fails Assemble after the sandbox is open.
			require.NoError(t, os.MkdirAll(filepath.Join(dir, pinFile), 0o700))

			_, err = Assemble(t.Context(), cfg)

			require.Error(t, err)
			_, stops, deletes := f.Counts()
			if existing {
				require.Equal(t, stops0+1, stops)
				require.Zero(t, deletes)
				return
			}
			require.Zero(t, stops)
			require.Equal(t, 1, deletes)
			_, ok, err := sandbox.ReadPin(dir)
			require.NoError(t, err)
			require.True(t, ok, "the pin is kept")
		})
	}
}

func TestAssembleMisconfiguredProviderCreatesNoSandbox(t *testing.T) {
	assembleHome(t)
	f := sandboxtest.New(t)
	cfg := sandboxCfg(t, f, "img:1")
	cfg.Provider = "openrouter"
	cfg.APIToken = ""
	t.Setenv("OPENROUTER_API_KEY", "")

	_, err := Assemble(t.Context(), cfg)
	require.Error(t, err)
	ensures, _, _ := f.Counts()
	require.Zero(t, ensures)
}

// writeMarkerMCP configures an MCP server that only touches marker when it starts.
func writeMarkerMCP(t *testing.T, marker string) {
	t.Helper()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable"), 0o700))
	cfg := fmt.Sprintf(`{"mcpServers":{"m":{"command":"sh","args":["-c","touch %s"],"timeoutSeconds":5}}}`, marker)
	require.NoError(t, os.WriteFile(filepath.Join(home, ".durable", "mcp.json"), []byte(cfg), 0o600))
}

func TestAssembleSandboxedSessionDiscoversMCPThroughTheWorker(t *testing.T) {
	assembleHome(t)
	writeStdioMCP(t)
	f := sandboxtest.New(t)

	a, err := Assemble(t.Context(), sandboxCfg(t, f, "img:1"))
	require.NoError(t, err)
	defer a.Close()

	require.Equal(t, 1, f.StdioStarts(), "discovery starts the server through the worker, not on the host")
	require.Len(t, a.mcp.Tools, 1)
	require.Equal(t, "mcp__m__where", a.mcp.Tools[0].Spec.Name)

	var tool durable.Tool
	for _, tl := range a.toolset {
		if tl.Spec().Name == "mcp__m__where" {
			tool = tl
		}
	}
	require.NotNil(t, tool)
	out, err := tool.Execute(t.Context(), durable.ToolCall{ID: "c1", Name: "mcp__m__where", Input: json.RawMessage(`{}`)})
	require.NoError(t, err)
	require.Contains(t, out, "cwd=")
}

func TestAssemblePreMSandboxedSessionStaysWithoutMCPTools(t *testing.T) {
	assembleHome(t)
	f := sandboxtest.New(t)
	cfg := sandboxCfg(t, f, "img:1")
	a, err := Assemble(t.Context(), cfg)
	require.NoError(t, err)
	a.Close()
	markExisting(t, cfg)
	dir := filepath.Dir(a.Path)
	require.NoError(t, os.Remove(filepath.Join(dir, mcpPinFile)))
	writeStdioMCP(t)

	a, err = Assemble(t.Context(), cfg)
	require.NoError(t, err)
	defer a.Close()

	require.Empty(t, a.mcp.Tools)
	require.Zero(t, f.StdioStarts())
}

func TestWritePinDerivesEgressFromHTTPServers(t *testing.T) {
	assembleHome(t)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable"), 0o700))
	cfg := `{"mcpServers":{
		"a":{"url":"https://mcp.example.com/v1","headers":{"X":"y"}},
		"b":{"url":"http://plain.example.com/x"},
		"c":{"url":"https://other.example.com:8443/x"},
		"d":{"command":"true"}}}`
	require.NoError(t, os.WriteFile(filepath.Join(home, ".durable", "mcp.json"), []byte(cfg), 0o600))

	a, err := Assemble(t.Context(), sandboxCfg(t, sandboxtest.New(t), "img:1"))
	require.NoError(t, err)
	defer a.Close()

	pin, ok, err := sandbox.ReadPin(filepath.Dir(a.Path))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []sandbox.Endpoint{
		{Host: "mcp.example.com", Port: 443},
		{Host: "other.example.com", Port: 8443},
		{Host: "plain.example.com", Port: 80},
	}, pin.Egress)
}

func TestStdioSpecRunsHTTPServersThroughTheBridge(t *testing.T) {
	spec := stdioSpec(mcp.Server{Type: "http", URL: "https://mcp.example.com/v1", Headers: map[string]string{"Authorization": "Bearer t"}})
	require.Equal(t, sandbox.WorkerBinary, spec.Command)
	require.Equal(t, []string{"mcp-http", "--url", "https://mcp.example.com/v1"}, spec.Args)
	require.JSONEq(t, `{"Authorization":"Bearer t"}`, spec.Env[headersEnv])

	spec = stdioSpec(mcp.Server{Type: "stdio", Command: "npx", Args: []string{"x"}, Env: map[string]string{"K": "v"}, UseWorkspaceCwd: true})
	require.Equal(t, sandbox.StdioSpec{Command: "npx", Args: []string{"x"}, Env: map[string]string{"K": "v"}, WorkspaceCwd: true}, spec)
}

func TestCappedWriterDropsWithOneMarker(t *testing.T) {
	var buf bytes.Buffer
	w := &cappedWriter{w: &buf, left: 5}
	for range 3 {
		n, err := w.Write([]byte("abcdef"))
		require.NoError(t, err)
		require.Equal(t, 6, n)
	}
	require.Equal(t, 1, strings.Count(buf.String(), "limit reached"))
	require.True(t, strings.HasPrefix(buf.String(), "abcde"))
}

func TestAssembleLocalSessionStillStartsMCPServers(t *testing.T) {
	assembleHome(t)
	marker := filepath.Join(t.TempDir(), "started")
	writeMarkerMCP(t, marker)

	a, err := Assemble(t.Context(), sandboxCfg(t, sandboxtest.New(t), ""))
	require.NoError(t, err)
	defer a.Close()

	require.FileExists(t, marker, "the control: a local session discovers MCP tools")
}
