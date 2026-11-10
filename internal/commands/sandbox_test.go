package commands

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/sandbox/sandboxtest"
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
