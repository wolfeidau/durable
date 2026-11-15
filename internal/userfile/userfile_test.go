package userfile

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestReadJSONPassthrough(t *testing.T) {
	p := write(t, t.TempDir(), "a.json", `{"a": 1}`)
	b, err := ReadJSON(p)
	require.NoError(t, err)
	require.Equal(t, `{"a": 1}`, string(b))
}

func TestReadJSONYAML(t *testing.T) {
	for _, name := range []string{"a.yaml", "a.yml"} {
		p := write(t, t.TempDir(), name, "$schema: x\nlist: [a, b]\nn:\n  k: 1\n")
		b, err := ReadJSON(p)
		require.NoError(t, err)
		require.JSONEq(t, `{"$schema":"x","list":["a","b"],"n":{"k":1}}`, string(b))
	}
}

func TestReadJSONEmpty(t *testing.T) {
	p := write(t, t.TempDir(), "a.yaml", "")
	b, err := ReadJSON(p)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(b))
}

func TestReadJSONMultiDoc(t *testing.T) {
	p := write(t, t.TempDir(), "a.yaml", "a: 1\n---\nb: 2\n")
	_, err := ReadJSON(p)
	require.ErrorContains(t, err, "more than one")
}

func TestReadJSONNonStringKey(t *testing.T) {
	p := write(t, t.TempDir(), "a.yaml", "outer:\n  1: x\n")
	_, err := ReadJSON(p)
	require.ErrorContains(t, err, "outer.1")
}

func TestReadJSONSyntaxError(t *testing.T) {
	p := write(t, t.TempDir(), "a.yaml", "a: [1\nb: 2\n")
	_, err := ReadJSON(p)
	require.ErrorContains(t, err, "line")
}

func TestReadJSONMissing(t *testing.T) {
	_, err := ReadJSON(filepath.Join(t.TempDir(), "a.yaml"))
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestFind(t *testing.T) {
	dir := t.TempDir()
	p, err := Find(dir, "mcp")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "mcp.json"), p)

	write(t, dir, "mcp.yaml", "")
	p, err = Find(dir, "mcp")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "mcp.yaml"), p)

	write(t, dir, "mcp.json", "{}")
	_, err = Find(dir, "mcp")
	require.ErrorContains(t, err, "mcp.json and mcp.yaml")
	require.ErrorContains(t, err, "keep one")
	require.NotContains(t, err.Error(), dir)
}
