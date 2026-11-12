// Package githosttest is a git host for tests whose repository is a local bare repository.
package githosttest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/githost"
	"github.com/wolfeidau/durable/internal/githost/github"
	"github.com/wolfeidau/durable/internal/sandbox"
)

// Host parses repositories as GitHub does but clones from Remote, a local bare repository.
type Host struct {
	*github.Host
	// Remote has a main branch holding main.txt and a feature branch that adds feature.txt.
	Remote string
}

var _ githost.Host = (*Host)(nil)

// Option adds content to the main branch, so the feature branch has it too.
type Option func(*config)

type config struct {
	files    map[string]string
	symlinks map[string]string
}

// WithFile adds a file at name, a slash path relative to the repository root.
func WithFile(name, content string) Option {
	return func(c *config) { c.files[name] = content }
}

// WithSymlink adds a symbolic link at name that points to target.
func WithSymlink(name, target string) Option {
	return func(c *config) { c.symlinks[name] = target }
}

// New creates the bare repository under t's temporary directory. It sets GIT_CONFIG_NOSYSTEM and GIT_CONFIG_GLOBAL for t, so
// the test doesn't read the developer's git configuration.
func New(t *testing.T, opts ...Option) *Host {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	work := filepath.Join(dir, "work")
	require.NoError(t, os.Mkdir(work, 0o700))
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...) //nolint:gosec // fixed git command in a test
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(work, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	cfg := config{files: map[string]string{}, symlinks: map[string]string{}}
	for _, o := range opts {
		o(&cfg)
	}
	run(dir, "init", "--bare", "-b", "main", remote)
	run(work, "init", "-b", "main")
	write("main.txt", "main\n")
	for name, content := range cfg.files {
		write(name, content)
	}
	for name, target := range cfg.symlinks {
		require.NoError(t, os.Symlink(target, filepath.Join(work, filepath.FromSlash(name))))
	}
	run(work, "add", ".")
	run(work, "commit", "-m", "main")
	run(work, "push", remote, "main")
	run(work, "checkout", "-b", "feature")
	write("feature.txt", "feature\n")
	run(work, "add", ".")
	run(work, "commit", "-m", "feature")
	run(work, "push", remote, "feature")
	return &Host{Host: github.New(""), Remote: remote}
}

// Access is GitHub's access with CloneURL pointing at Remote.
func (h *Host) Access(repo githost.Repo) sandbox.GitAccess {
	a := h.Host.Access(repo)
	a.CloneURL = h.Remote
	return a
}
