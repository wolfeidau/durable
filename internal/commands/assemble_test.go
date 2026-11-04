package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
)

func assembleHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func TestAssembleExistingOnlyMissingSession(t *testing.T) {
	assembleHome(t)
	cwd := t.TempDir()

	_, err := Assemble(context.Background(), AssembleConfig{CWD: cwd, Session: "nope", APIToken: "k", ExistingOnly: true})
	require.ErrorIs(t, err, ErrNoSession)

	path, err := SessionPath(cwd, "nope")
	require.NoError(t, err)
	_, err = os.Stat(filepath.Dir(path))
	require.ErrorIs(t, err, os.ErrNotExist, "no session directory, pin or session.json may be written")
}

func TestAssembleNonInteractiveDefersApprovals(t *testing.T) {
	assembleHome(t)
	a, err := Assemble(context.Background(), AssembleConfig{CWD: t.TempDir(), Session: "s", APIToken: "k"})
	require.NoError(t, err)
	defer a.Close()

	require.True(t, a.Options.DeferApprovals)
	require.Nil(t, a.Options.Ask)
	require.NotNil(t, a.Options.Gate, "read-only gate without --yes")
	require.Nil(t, a.Options.OnDelta)
}

func TestAssembleYesNonInteractiveDoesNotDefer(t *testing.T) {
	assembleHome(t)
	a, err := Assemble(context.Background(), AssembleConfig{CWD: t.TempDir(), Session: "s", APIToken: "k", Yes: true})
	require.NoError(t, err)
	defer a.Close()

	require.Nil(t, a.Options.Gate)
	require.False(t, a.Options.DeferApprovals, "a nil gate would park every call")
}

func TestAssembleYesNonInteractiveWithHooksDefersAsk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable"), 0o700))
	cfg := `{"hooks":{"PreToolUse":[{"matcher":"bash","hooks":[{"type":"command","command":"echo '{\"hookSpecificOutput\":{\"permissionDecision\":\"ask\"}}'"}]}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(home, ".durable", "hooks.json"), []byte(cfg), 0o600))

	a, err := Assemble(context.Background(), AssembleConfig{CWD: t.TempDir(), Session: "s", APIToken: "k", Yes: true})
	require.NoError(t, err)
	defer a.Close()

	require.True(t, a.Options.DeferApprovals)
	require.NotNil(t, a.Options.Gate)
	d, err := a.Options.Gate(context.Background(), durable.ToolCall{ID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)})
	require.NoError(t, err)
	require.Equal(t, durable.ActionAsk, d.Action)
}

func TestAssembleInteractiveKeepsApprovalsInline(t *testing.T) {
	assembleHome(t)
	a, err := Assemble(context.Background(), AssembleConfig{CWD: t.TempDir(), Session: "s", APIToken: "k", Interactive: true})
	require.NoError(t, err)
	defer a.Close()

	require.False(t, a.Options.DeferApprovals)
}

func TestAssembleChainsCallerObserver(t *testing.T) {
	assembleHome(t)
	srv := modelsServer(t, 200, `{"data":[{"id":"vendor/model","context_length":1000}]}`)
	t.Setenv("OPENROUTER_BASE_URL", srv.URL)

	var out bytes.Buffer
	var got []durable.EventKind
	a, err := Assemble(context.Background(), AssembleConfig{
		CWD:      t.TempDir(),
		Session:  "s",
		Provider: "openrouter",
		Model:    "vendor/model",
		APIToken: "k",
		Out:      &out,
		Observer: func(e durable.Event) { got = append(got, e.Kind) },
	})
	require.NoError(t, err)
	defer a.Close()

	require.NotNil(t, a.Options.Compactor)
	require.Contains(t, a.CompactionNotice, "[compaction]")
	a.Options.Observer(durable.Event{Kind: durable.EventCompacted})
	require.Equal(t, []durable.EventKind{durable.EventCompacted}, got)
	require.Contains(t, out.String(), "[compacting history]")
}
