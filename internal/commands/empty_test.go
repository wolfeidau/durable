package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/sandbox/sandboxtest"
)

func emptyCfg(f *sandboxtest.Fake, image string) AssembleConfig {
	return AssembleConfig{
		Workspace: WorkspaceEmpty, SessionID: EmptySessionID("req-1"), Session: "s", APIToken: "k",
		Sandbox: &SandboxConfig{Backend: f, Image: image},
	}
}

// readSessionMetaFile reads session.json without the database ReadSessionMeta requires.
func readSessionMetaFile(t *testing.T, dir string) SessionMeta {
	t.Helper()
	m, ok, err := readJSONPin[SessionMeta](filepath.Join(dir, sessionMetaFile), "session metadata")
	require.NoError(t, err)
	require.True(t, ok)
	return m
}

func TestEmptySessionIDIsStableAndPerRequest(t *testing.T) {
	require.Equal(t, "e813e240a8c8f3d5", EmptySessionID("a"), "existing sessions depend on the derivation")
	require.NotEqual(t, EmptySessionID("a"), EmptySessionID("b"))
	require.Regexp(t, `^[0-9a-f]{16}$`, EmptySessionID("a"))
}

func TestAssembleEmptySessionIsSandboxedWithNoCWD(t *testing.T) {
	assembleHome(t)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	cfg := emptyCfg(sandboxtest.New(t), "img:1")

	a, err := Assemble(t.Context(), cfg)
	require.NoError(t, err)
	defer a.Close()

	require.Equal(t, filepath.Join(home, ".durable", "sessions", cfg.SessionID, "session.sqlite"), a.Path)
	meta := readSessionMetaFile(t, filepath.Dir(a.Path))
	require.Equal(t, WorkspaceEmpty, meta.Workspace)
	require.Empty(t, meta.CWD)
	require.NotEmpty(t, a.toolset)
	for _, tl := range a.toolset {
		require.Contains(t, fmt.Sprintf("%T", tl), "remoteTool")
	}
}

func TestAssembleEmptySessionNeedsSandbox(t *testing.T) {
	assembleHome(t)
	f := sandboxtest.New(t)

	_, err := Assemble(t.Context(), emptyCfg(f, ""))
	require.ErrorIs(t, err, ErrEmptyNeedsSandbox)

	cfg := emptyCfg(f, "img:1")
	cfg.Sandbox = nil
	_, err = Assemble(t.Context(), cfg)
	require.ErrorIs(t, err, ErrEmptyNeedsSandbox)
}

func TestAssembleEmptySessionPinsUserContextNotProcessCWD(t *testing.T) {
	assembleHome(t)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	writeFile(t, filepath.Join(home, ".durable", "AGENTS.md"), "user rules")
	writeFile(t, filepath.Join(home, ".durable", "skills", "mine", "SKILL.md"), "---\nname: mine\ndescription: d\n---\nbody")

	proc := t.TempDir()
	writeFile(t, filepath.Join(proc, "AGENTS.md"), "process cwd rules")
	writeFile(t, filepath.Join(proc, ".durable", "skills", "theirs", "SKILL.md"), "---\nname: theirs\ndescription: d\n---\nbody")
	t.Chdir(proc)

	a, err := Assemble(t.Context(), emptyCfg(sandboxtest.New(t), "img:1"))
	require.NoError(t, err)
	defer a.Close()

	require.Contains(t, a.instructions.Text, "user rules")
	require.NotContains(t, a.instructions.Text, "process cwd rules")
	require.Len(t, a.skills.Skills, 1)
	require.Equal(t, "mine", a.skills.Skills[0].Name)
	require.False(t, a.skills.Skills[0].Sandbox, "an empty session has no clone to read project files from")
}

func TestSessionMetaWithoutWorkspaceIsDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, sessionMetaFile), `{"cwd":"/a","name":"n","created_at":"2026-01-02T03:04:05Z"}`)
	m := readSessionMetaFile(t, dir)
	require.Empty(t, m.Workspace)
	require.Equal(t, "/a", m.CWD)
}
