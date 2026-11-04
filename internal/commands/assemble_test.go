package commands

import (
	"bytes"
	"context"
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
