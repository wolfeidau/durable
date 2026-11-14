package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/claude"
	"github.com/wolfeidau/durable/internal/catalog"
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

func TestAssembleReadsConfigFromHome(t *testing.T) {
	assembleHome(t)
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable"), 0o700))
	cfg := `{"hooks":{"PreToolUse":[{"matcher":"bash","hooks":[{"type":"command","command":"echo '{\"hookSpecificOutput\":{\"permissionDecision\":\"ask\"}}'"}]}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(home, ".durable", "hooks.json"), []byte(cfg), 0o600))

	a, err := Assemble(context.Background(), AssembleConfig{CWD: t.TempDir(), Session: "s", APIToken: "k", Yes: true, Home: home})
	require.NoError(t, err)
	defer a.Close()

	require.True(t, a.Options.DeferApprovals, "hooks.json under Home, not HOME, must apply")
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"vendor/model","context_length":1000}]}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENROUTER_BASE_URL", srv.URL)

	out := &lockedBuffer{}
	var got []durable.EventKind
	a, err := Assemble(context.Background(), AssembleConfig{
		CWD:      t.TempDir(),
		Session:  "s",
		Provider: "openrouter",
		Model:    "vendor/model",
		APIToken: "k",
		Out:      out,
		Observer: func(e durable.Event) { got = append(got, e.Kind) },
	})
	require.NoError(t, err)
	defer a.Close()

	require.NotNil(t, a.Options.Compactor)
	require.Eventually(t, func() bool { return strings.Contains(out.String(), "[compaction] context window 1000 tokens") }, 5*time.Second, 5*time.Millisecond)
	a.Options.Observer(durable.Event{Kind: durable.EventCompacted})
	require.Equal(t, []durable.EventKind{durable.EventCompacted}, got)
	require.Contains(t, out.String(), "[compacting history]")
}

func TestLookupNoticeLogsAFailure(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	var out bytes.Buffer

	lookupNotice(&out, logger, route{Provider: providerAnthropic, Protocol: protocolMessages})(catalog.Info{}, errors.New("failed to list models: refused"))

	require.Contains(t, out.String(), "[usage] warning: failed to list models: refused")
	require.Contains(t, logs.String(), "model lookup failed")
	require.Contains(t, logs.String(), "model="+claude.DefaultModel)

	logs.Reset()
	lookupNotice(&out, logger, route{Provider: providerOpenRouter, Protocol: protocolResponses, Model: "m"})(catalog.Info{ContextWindow: 10}, nil)
	require.Empty(t, logs.String(), "a successful lookup logs nothing")
}
