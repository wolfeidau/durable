package commands

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/githost"
	"github.com/wolfeidau/durable/internal/githost/githosttest"
	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/sandbox/sandboxtest"
)

func repoCfg(t *testing.T, f *sandboxtest.Fake, h *githosttest.Host, branch string, tokens bool) AssembleConfig {
	t.Helper()
	repo, err := h.ParseRepo("github.com/octo/hello")
	require.NoError(t, err)
	return AssembleConfig{
		Workspace: WorkspaceRepo, SessionID: RepoSessionID(repo.String(), branch, "default"), Session: "default", APIToken: "k",
		Repo: repo, Branch: branch,
		Sandbox: &SandboxConfig{
			Backend: f, Environments: imageEnvs("img:1"),
			GitHosts:  map[string]githost.Host{"github": h},
			GitTokens: map[string]bool{"github": tokens},
		},
	}
}

func repoPin(t *testing.T, a *Assembled) sandbox.Pin {
	t.Helper()
	pin, ok, err := sandbox.ReadPin(filepath.Dir(a.Path))
	require.NoError(t, err)
	require.True(t, ok)
	return pin
}

func TestAssembleRepoSessionClonesTheBranch(t *testing.T) {
	assembleHome(t)
	f := sandboxtest.New(t)
	h := githosttest.New(t)
	cfg := repoCfg(t, f, h, "feature", true)

	a, err := Assemble(t.Context(), cfg)
	require.NoError(t, err)
	defer a.Close()

	pin := repoPin(t, a)
	root := f.Root(pin.Name)
	require.FileExists(t, filepath.Join(root, "main.txt"))
	require.FileExists(t, filepath.Join(root, "feature.txt"))
	meta := readSessionMetaFile(t, filepath.Dir(a.Path))
	require.Equal(t, WorkspaceRepo, meta.Workspace)
	require.Equal(t, "github.com/octo/hello", meta.Repo)
	require.Equal(t, "feature", meta.Branch)
	require.Empty(t, meta.CWD)
	require.Len(t, pin.Git, 1)
	require.Equal(t, "durable-github", pin.Git[0].Provider)
	require.Equal(t, h.Remote, pin.Git[0].CloneURL)
}

func TestAssembleRepoSessionReopenClonesOnlyAnEmptyWorkspace(t *testing.T) {
	assembleHome(t)
	f := sandboxtest.New(t)
	h := githosttest.New(t)
	cfg := repoCfg(t, f, h, "main", true)
	a, err := Assemble(t.Context(), cfg)
	require.NoError(t, err)
	pin := repoPin(t, a)
	root := f.Root(pin.Name)
	a.Close()
	// Mark the session as opened before, as durable.Open would.
	require.NoError(t, os.WriteFile(a.Path, nil, 0o600))
	mainFile := filepath.Join(root, "main.txt")
	require.NoError(t, os.WriteFile(mainFile, []byte("edited\n"), 0o600))

	a, err = Assemble(t.Context(), cfg)
	require.NoError(t, err)
	b, err := os.ReadFile(mainFile)
	require.NoError(t, err)
	require.Equal(t, "edited\n", string(b), "a workspace with files is not cloned over")
	a.Close()

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	for _, e := range entries {
		require.NoError(t, os.RemoveAll(filepath.Join(root, e.Name())))
	}
	a, err = Assemble(t.Context(), cfg)
	require.NoError(t, err)
	defer a.Close()
	b, err = os.ReadFile(mainFile)
	require.NoError(t, err)
	require.Equal(t, "main\n", string(b), "an emptied workspace is cloned again")
}

func TestAssembleRepoSessionMissingBranchDiscardsTheNewSandbox(t *testing.T) {
	assembleHome(t)
	f := sandboxtest.New(t)
	h := githosttest.New(t)
	cfg := repoCfg(t, f, h, "nope", true)

	_, err := Assemble(t.Context(), cfg)

	var ce *CloneError
	require.ErrorAs(t, err, &ce)
	require.Contains(t, ce.Msg, "nope")
	require.NotContains(t, ce.Msg, "\n")
	_, _, deletes := f.Counts()
	require.Equal(t, 1, deletes)
	dir := filepath.Dir(sessionDBPath(t, cfg))
	require.FileExists(t, filepath.Join(dir, sandbox.PinFile))
	require.FileExists(t, filepath.Join(dir, sessionMetaFile))

	cfg.Branch = "main"
	cfg.SessionID = RepoSessionID(cfg.Repo.String(), "nope", "default")
	a, err := Assemble(t.Context(), cfg)
	require.NoError(t, err, "a retry reuses the pin and clones afresh")
	defer a.Close()
	require.FileExists(t, filepath.Join(f.Root(repoPin(t, a).Name), "main.txt"))
}

func TestAssembleRepoSessionWithoutTokenIsAnonymous(t *testing.T) {
	assembleHome(t)
	f := sandboxtest.New(t)
	h := githosttest.New(t)

	a, err := Assemble(t.Context(), repoCfg(t, f, h, "main", false))
	require.NoError(t, err)
	defer a.Close()

	g := repoPin(t, a).Git[0]
	require.Empty(t, g.Provider)
	require.Empty(t, g.ProfileID)
	require.Empty(t, g.CredentialEnv)
	require.Empty(t, g.GitConfig)
	require.NotEmpty(t, g.Rules)
	require.Equal(t, h.Remote, g.CloneURL)
}

func TestAssembleRepoSessionNeedsSandbox(t *testing.T) {
	assembleHome(t)
	cfg := repoCfg(t, sandboxtest.New(t), githosttest.New(t), "main", true)
	cfg.Sandbox.Environments = nil

	_, err := Assemble(t.Context(), cfg)

	require.ErrorIs(t, err, ErrEmptyNeedsSandbox)
}

func sessionDBPath(t *testing.T, cfg AssembleConfig) string {
	t.Helper()
	p, err := cfg.sessionPath()
	require.NoError(t, err)
	return p
}

func TestRepoSessionIDIsStableAndDistinct(t *testing.T) {
	id := RepoSessionID("github.com/o/n", "main", "default")
	require.Equal(t, id, RepoSessionID("github.com/o/n", "main", "default"))
	require.Regexp(t, `^[0-9a-f]{16}$`, id)
	require.NotEqual(t, id, RepoSessionID("github.com/o/n", "dev", "default"))
	require.NotEqual(t, id, RepoSessionID("github.com/o/n", "main", "other"))
	require.NotEqual(t, id, RepoSessionID("github.com/o/m", "main", "default"))
	require.NotEqual(t, id, EmptySessionID("github.com/o/n\x00main\x00default"))
	require.NotEqual(t, id, EmptySessionID("repo\x00github.com/o/n\x00main\x00default"))
	sum := sha256.Sum256([]byte("/x\x00default"))
	require.NotEqual(t, id, hex.EncodeToString(sum[:])[:16])
}

func TestLastLineIsSanitisedAndCapped(t *testing.T) {
	require.Empty(t, lastLine(nil))
	require.Equal(t, "fatal: bad", lastLine([]byte("warning: x\nfatal: bad\n\n")))
	require.Equal(t, "[31mevil", lastLine([]byte("a\n\x1b[31mev\x00il\x07\r\n")))
	long := lastLine([]byte(strings.Repeat("é", 300)))
	require.LessOrEqual(t, len(long), 200)
	require.True(t, strings.HasPrefix(long, "é"))
}

func TestAssembleRepoSessionRetriesAnInterruptedClone(t *testing.T) {
	assembleHome(t)
	f := sandboxtest.New(t)
	h := githosttest.New(t)
	cfg := repoCfg(t, f, h, "main", true)
	a, err := Assemble(t.Context(), cfg)
	require.NoError(t, err)
	pin := repoPin(t, a)
	root := f.Root(pin.Name)
	a.Close()
	require.NoError(t, os.WriteFile(a.Path, nil, 0o600))

	// A clone killed partway leaves only its staging directory behind.
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	for _, e := range entries {
		require.NoError(t, os.RemoveAll(filepath.Join(root, e.Name())))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".durable-clone", ".git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".durable-clone", "partial"), nil, 0o600))

	a, err = Assemble(t.Context(), cfg)
	require.NoError(t, err)
	defer a.Close()
	require.FileExists(t, filepath.Join(root, "main.txt"), "the interrupted clone is retried")
	require.DirExists(t, filepath.Join(root, ".git"))
	require.NoDirExists(t, filepath.Join(root, ".durable-clone"))
	require.NoFileExists(t, filepath.Join(root, "partial"))
}
