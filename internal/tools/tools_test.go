package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/wolfeidau/durable"
)

type env struct {
	root  string
	state string
	tools map[string]durable.Tool
}

// bashState is the state file for the call ID that runCtx uses.
func (e env) bashState() string { return statePath(filepath.Join(e.state, "bash"), "1") }

func newEnv(t *testing.T) env {
	t.Helper()
	root := t.TempDir()
	state := t.TempDir()
	all, err := All(root, state)
	require.NoError(t, err)
	m := map[string]durable.Tool{}
	for _, tl := range all {
		m[tl.Spec().Name] = tl
	}
	real, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	return env{root: real, state: state, tools: m}
}

func (e env) run(t *testing.T, name string, in map[string]any) (string, error) {
	t.Helper()
	return e.runCtx(t, context.Background(), name, in)
}

func (e env) runCtx(t *testing.T, ctx context.Context, name string, in map[string]any) (string, error) {
	t.Helper()
	b, err := json.Marshal(in)
	require.NoError(t, err)
	return e.tools[name].Execute(ctx, durable.ToolCall{ID: "1", Name: name, Input: b})
}

func (e env) write(t *testing.T, rel, content string) string {
	t.Helper()
	p := filepath.Join(e.root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

func TestAllOrderAndSchemas(t *testing.T) {
	root := t.TempDir()
	all, err := All(root, t.TempDir())
	require.NoError(t, err)

	var names []string
	for _, tl := range all {
		names = append(names, tl.Spec().Name)
		var schema map[string]any
		require.NoError(t, json.Unmarshal(tl.Spec().Schema, &schema), tl.Spec().Name)
		require.Equal(t, "object", schema["type"])
		require.Equal(t, false, schema["additionalProperties"])
		props := schema["properties"].(map[string]any)
		var required []string
		for _, r := range schema["required"].([]any) {
			required = append(required, r.(string))
		}
		require.Len(t, required, len(props), tl.Spec().Name)
		for _, r := range required {
			require.Contains(t, props, r)
		}
	}
	require.Equal(t, []string{"read_file", "list_files", "grep", "edit_file", "bash"}, names)
	require.Equal(t, durable.ReplaySafe, all[0].Replay())
	require.Equal(t, durable.ReplaySafe, all[1].Replay())
	require.Equal(t, durable.ReplaySafe, all[2].Replay())
	require.Equal(t, durable.ReplayNever, all[3].Replay())
	require.Equal(t, durable.ReplayNever, all[4].Replay())
}

func TestUnknownFieldsRejected(t *testing.T) {
	e := newEnv(t)
	_, err := e.run(t, "read_file", map[string]any{"path": "a", "offset": nil, "limit": nil, "extra": 1})
	require.ErrorContains(t, err, "invalid tool input")
}

func TestPathEscapes(t *testing.T) {
	e := newEnv(t)
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("secret"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(e.root, "linkdir")))
	require.NoError(t, os.Symlink(outsideFile, filepath.Join(e.root, "linkfile")))

	for _, p := range []string{
		"../x",
		"sub/../../x",
		outsideFile,
		"linkdir/secret.txt",
		"linkfile",
	} {
		_, err := e.run(t, "read_file", map[string]any{"path": p, "offset": nil, "limit": nil})
		require.ErrorContains(t, err, "outside the workspace root", p)
	}

	// Create through a symlinked parent must not write outside.
	_, err := e.run(t, "edit_file", map[string]any{"path": "linkdir/new/file.txt", "old_string": "", "new_string": "x"})
	require.ErrorContains(t, err, "outside the workspace root")
	_, statErr := os.Stat(filepath.Join(outside, "new"))
	require.True(t, os.IsNotExist(statErr))

	_, err = e.run(t, "list_files", map[string]any{"path": "linkdir"})
	require.ErrorContains(t, err, "outside the workspace root")
}

func TestPathInsideRootAllowed(t *testing.T) {
	e := newEnv(t)
	p := e.write(t, "a/b.txt", "hi\n")
	for _, in := range []string{"a/b.txt", p, "a/../a/b.txt"} {
		out, err := e.run(t, "read_file", map[string]any{"path": in, "offset": nil, "limit": nil})
		require.NoError(t, err, in)
		require.Contains(t, out, "hi")
	}
	_, err := e.run(t, "list_files", map[string]any{"path": "."})
	require.NoError(t, err)
}

func TestReadFile(t *testing.T) {
	e := newEnv(t)
	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&sb, "line%d\n", i)
	}
	e.write(t, "f.txt", sb.String())

	out, err := e.run(t, "read_file", map[string]any{"path": "f.txt", "offset": 3, "limit": 2})
	require.NoError(t, err)
	require.Contains(t, out, "     3\tline3\n")
	require.Contains(t, out, "     4\tline4\n")
	require.NotContains(t, out, "line5\n")
	require.Contains(t, out, "offset=5")

	out, err = e.run(t, "read_file", map[string]any{"path": "f.txt", "offset": 9, "limit": nil})
	require.NoError(t, err)
	require.Contains(t, out, "line10")
	require.NotContains(t, out, "truncated")

	_, err = e.run(t, "read_file", map[string]any{"path": "f.txt", "offset": 50, "limit": nil})
	require.ErrorContains(t, err, "beyond the end")

	_, err = e.run(t, "read_file", map[string]any{"path": "f.txt", "offset": 0, "limit": nil})
	require.Error(t, err)
}

func TestReadFileDefaultLimit(t *testing.T) {
	e := newEnv(t)
	e.write(t, "big.txt", strings.Repeat("x\n", 2500))
	out, err := e.run(t, "read_file", map[string]any{"path": "big.txt", "offset": nil, "limit": nil})
	require.NoError(t, err)
	require.Contains(t, out, "offset=2001")
}

func TestReadFileBinary(t *testing.T) {
	e := newEnv(t)
	e.write(t, "b.bin", "abc\x00def")
	_, err := e.run(t, "read_file", map[string]any{"path": "b.bin", "offset": nil, "limit": nil})
	require.ErrorContains(t, err, "binary")
}

func TestListFiles(t *testing.T) {
	e := newEnv(t)
	e.write(t, "b.txt", "")
	e.write(t, "a/c.txt", "")
	e.write(t, ".git/config", "")

	out, err := e.run(t, "list_files", map[string]any{"path": nil})
	require.NoError(t, err)
	require.Equal(t, "a/\na/c.txt\nb.txt\n", out)
}

func TestListFilesCap(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < maxListEntries+5; i++ {
		e.write(t, fmt.Sprintf("d/f%05d", i), "")
	}
	out, err := e.run(t, "list_files", map[string]any{"path": "d"})
	require.NoError(t, err)
	require.Contains(t, out, "[truncated")
	require.Equal(t, maxListEntries+1, strings.Count(out, "\n"))
}

func TestGrep(t *testing.T) {
	e := newEnv(t)
	e.write(t, "a.go", "foo\nbar\nfoo bar\n")
	e.write(t, "b.txt", "foo\n")
	e.write(t, ".git/x", "foo\n")
	e.write(t, "bin.dat", "foo\x00")

	out, err := e.run(t, "grep", map[string]any{"pattern": "foo", "path": nil, "glob": nil})
	require.NoError(t, err)
	require.Equal(t, "a.go:1:foo\na.go:3:foo bar\nb.txt:1:foo\n", out)

	out, err = e.run(t, "grep", map[string]any{"pattern": "foo", "path": nil, "glob": "*.go"})
	require.NoError(t, err)
	require.Equal(t, "a.go:1:foo\na.go:3:foo bar\n", out)

	out, err = e.run(t, "grep", map[string]any{"pattern": "nomatch", "path": "a.go", "glob": nil})
	require.NoError(t, err)
	require.Equal(t, "no matches", out)

	_, err = e.run(t, "grep", map[string]any{"pattern": "(", "path": nil, "glob": nil})
	require.ErrorContains(t, err, "invalid pattern")
}

func TestGrepCap(t *testing.T) {
	e := newEnv(t)
	e.write(t, "m.txt", strings.Repeat("hit\n", maxGrepMatches+10))
	out, err := e.run(t, "grep", map[string]any{"pattern": "hit", "path": nil, "glob": nil})
	require.NoError(t, err)
	require.Contains(t, out, "[truncated")
	require.Equal(t, maxGrepMatches+1, strings.Count(out, "\n"))
}

func TestGrepHonoursContext(t *testing.T) {
	e := newEnv(t)
	e.write(t, "a.txt", "foo\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.runCtx(t, ctx, "grep", map[string]any{"pattern": "foo", "path": nil, "glob": nil})
	require.ErrorIs(t, err, context.Canceled)
}

func TestEditFile(t *testing.T) {
	e := newEnv(t)
	p := e.write(t, "f.txt", "one two three")
	require.NoError(t, os.Chmod(p, 0o750))

	_, err := e.run(t, "edit_file", map[string]any{"path": "f.txt", "old_string": "two", "new_string": "2"})
	require.NoError(t, err)
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	require.Equal(t, "one 2 three", string(data))
	info, err := os.Stat(p)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o750), info.Mode().Perm())

	entries, err := os.ReadDir(e.root)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temp files left behind")
}

func TestEditFileMatchCounts(t *testing.T) {
	e := newEnv(t)
	p := e.write(t, "f.txt", "a b a")

	_, err := e.run(t, "edit_file", map[string]any{"path": "f.txt", "old_string": "zzz", "new_string": "x"})
	require.ErrorContains(t, err, "0 matches")

	_, err = e.run(t, "edit_file", map[string]any{"path": "f.txt", "old_string": "a", "new_string": "x"})
	require.ErrorContains(t, err, "matches 2 places")

	data, err := os.ReadFile(p)
	require.NoError(t, err)
	require.Equal(t, "a b a", string(data))
}

func TestEditFileCreate(t *testing.T) {
	e := newEnv(t)
	_, err := e.run(t, "edit_file", map[string]any{"path": "x/y/new.txt", "old_string": "", "new_string": "hello"})
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(e.root, "x/y/new.txt"))
	require.NoError(t, err)
	require.Equal(t, "hello", string(data))

	_, err = e.run(t, "edit_file", map[string]any{"path": "x/y/new.txt", "old_string": "", "new_string": "again"})
	require.ErrorContains(t, err, "already exists")
	data, err = os.ReadFile(filepath.Join(e.root, "x/y/new.txt"))
	require.NoError(t, err)
	require.Equal(t, "hello", string(data))
}
