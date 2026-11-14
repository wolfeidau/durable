package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/sandbox/sandboxtest"
)

func approvalsCfg(f *sandboxtest.Fake, approvals string) AssembleConfig {
	cfg := emptyCfg(f, "")
	cfg.Sandbox.Environments = map[string]Environment{"go": {Name: "go", Image: "img:1", Approvals: approvals}}
	cfg.Environment = "go"
	return cfg
}

func bashCall() durable.ToolCall {
	return durable.ToolCall{ID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}
}

func TestLoadEnvironmentsApprovals(t *testing.T) {
	envs, err := LoadEnvironments(writeEnvFile(t, `{"a": {"image": "i", "approvals": "auto"}, "b": {"image": "i", "approvals": "ask"}, "c": {"image": "i"}}`))
	require.NoError(t, err)
	require.Equal(t, ApprovalsAuto, envs["a"].Approvals)
	require.Equal(t, ApprovalsAsk, envs["b"].Approvals)
	require.Empty(t, envs["c"].Approvals)

	_, err = LoadEnvironments(writeEnvFile(t, `{"a": {"image": "i", "approvals": "yes"}}`))
	require.ErrorContains(t, err, "approvals must be")
}

func TestAssembleAutoApprovalsSkipsTheGateInASandboxedSession(t *testing.T) {
	assembleHome(t)
	a, err := Assemble(t.Context(), approvalsCfg(sandboxtest.New(t), ApprovalsAuto))
	require.NoError(t, err)
	defer a.Close()

	require.Nil(t, a.Options.Gate)
	require.False(t, a.Options.DeferApprovals)
}

func TestAssembleAskOrUnsetApprovalsKeepTheGate(t *testing.T) {
	for _, approvals := range []string{"", ApprovalsAsk} {
		t.Run("approvals="+approvals, func(t *testing.T) {
			assembleHome(t)
			a, err := Assemble(t.Context(), approvalsCfg(sandboxtest.New(t), approvals))
			require.NoError(t, err)
			defer a.Close()

			require.NotNil(t, a.Options.Gate)
			require.True(t, a.Options.DeferApprovals)
			d, err := a.Options.Gate(t.Context(), bashCall())
			require.NoError(t, err)
			require.Equal(t, durable.ActionAsk, d.Action)
		})
	}
}

func TestAssembleApprovalsIsReadOnEachOpenNotPinned(t *testing.T) {
	assembleHome(t)
	f := sandboxtest.New(t)
	cfg := approvalsCfg(f, ApprovalsAuto)
	a, err := Assemble(t.Context(), cfg)
	require.NoError(t, err)
	a.Close()
	require.NoError(t, os.WriteFile(a.Path, nil, 0o600))

	for name, tc := range map[string]struct {
		envs    map[string]Environment
		wantNil bool
	}{
		"tightened":   {map[string]Environment{"go": {Image: "img:1", Approvals: ApprovalsAsk}}, false},
		"missing env": {nil, false},
		"still auto":  {map[string]Environment{"go": {Image: "img:1", Approvals: ApprovalsAuto}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			again := cfg
			again.Sandbox = &SandboxConfig{Backend: f, Environments: tc.envs}
			a, err := Assemble(t.Context(), again)
			require.NoError(t, err)
			defer a.Close()

			require.Equal(t, tc.wantNil, a.Options.Gate == nil)
		})
	}
}

func TestAssembleDirectorySessionIgnoresAutoApprovals(t *testing.T) {
	assembleHome(t)
	cfg := sandboxCfg(t, sandboxtest.New(t), "")
	cfg.Sandbox.Environments = map[string]Environment{"default": {Name: "default", Image: "img:1", Approvals: ApprovalsAuto}}

	a, err := Assemble(t.Context(), cfg)
	require.NoError(t, err)
	defer a.Close()

	require.NotNil(t, a.Options.Gate)
	require.True(t, a.Options.DeferApprovals)
}

func TestAssembleAutoApprovalsStillLetsAHookDeny(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable"), 0o700))
	hooks := `{"hooks":{"PreToolUse":[{"matcher":"bash","hooks":[{"type":"command","command":"echo '{\"hookSpecificOutput\":{\"permissionDecision\":\"deny\"}}'"}]}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(home, ".durable", "hooks.json"), []byte(hooks), 0o600))

	a, err := Assemble(t.Context(), approvalsCfg(sandboxtest.New(t), ApprovalsAuto))
	require.NoError(t, err)
	defer a.Close()

	require.NotNil(t, a.Options.Gate)
	d, err := a.Options.Gate(t.Context(), bashCall())
	require.NoError(t, err)
	require.Equal(t, durable.ActionDeny, d.Action)
}

func TestAssembleSessionWithNoRecordedEnvironmentAsks(t *testing.T) {
	assembleHome(t)
	cfg := approvalsCfg(sandboxtest.New(t), ApprovalsAuto)
	cfg.Sandbox.Environments = map[string]Environment{DefaultEnvironment: {Name: DefaultEnvironment, Image: "img:1", Approvals: ApprovalsAuto}}
	cfg.Environment = ""

	a, err := Assemble(t.Context(), cfg)
	require.NoError(t, err)
	defer a.Close()

	require.NotNil(t, a.Options.Gate, "an unnamed environment is never resolved to today's default for approvals")
}
