package userfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// load writes body as ~/.durable/name under a fresh home and loads its base.
func load(t *testing.T, name, body string) (File, error) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".durable")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	return Load(home, strings.TrimSuffix(name, filepath.Ext(name)))
}

func TestLoadJSONPassthrough(t *testing.T) {
	f, err := load(t, "a.json", `{"a": 1}`)
	require.NoError(t, err)
	require.Equal(t, `{"a": 1}`, string(f.Data))
	require.Equal(t, "a.json", f.Name)
	require.Equal(t, "~/.durable/a.json", f.Label)
}

func TestLoadYAML(t *testing.T) {
	for _, name := range []string{"a.yaml", "a.yml"} {
		f, err := load(t, name, "$schema: x\nlist: [a, b]\nn:\n  k: 1\n")
		require.NoError(t, err)
		require.JSONEq(t, `{"$schema":"x","list":["a","b"],"n":{"k":1}}`, string(f.Data))
		require.Equal(t, name, f.Name)
		require.Equal(t, "~/.durable/"+name, f.Label)
	}
}

func yamlErr(t *testing.T, body string) error {
	t.Helper()
	_, err := load(t, "a.yaml", body)
	require.Error(t, err)
	return err
}

func TestLoadEmptyYAML(t *testing.T) {
	f, err := load(t, "a.yaml", "")
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(f.Data))
}

func TestLoadMultiDoc(t *testing.T) {
	require.ErrorContains(t, yamlErr(t, "a: 1\n---\nb: 2\n"), "more than one")
}

func TestLoadNonStringKey(t *testing.T) {
	require.ErrorContains(t, yamlErr(t, "outer:\n  1: x\n"), "outer.1")
}

func TestLoadSyntaxErrorNamesLabelOnce(t *testing.T) {
	err := yamlErr(t, "a: [1\nb: 2\n")
	require.ErrorContains(t, err, "failed to parse ~/.durable/a.yaml: yaml: line")
	require.Equal(t, 1, strings.Count(err.Error(), "a.yaml"))
}

func TestLoadMissing(t *testing.T) {
	f, err := Load(t.TempDir(), "mcp")
	require.NoError(t, err)
	require.Nil(t, f.Data)
	require.Equal(t, "mcp.json", f.Name)
	require.Equal(t, "~/.durable/mcp.json", f.Label)
}

func TestLoadReadErrorHidesHome(t *testing.T) {
	home := t.TempDir()
	// A directory where the file should be makes the read fail without being missing.
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable", "mcp.json"), 0o700))
	_, err := Load(home, "mcp")
	require.ErrorContains(t, err, "failed to read ~/.durable/mcp.json")
	require.NotContains(t, err.Error(), home)
}

func TestLoadBothFormsExist(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".durable")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mcp.yaml"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mcp.json"), []byte("{}"), 0o600))
	_, err := Load(home, "mcp")
	require.ErrorContains(t, err, "~/.durable: both mcp.json and mcp.yaml exist; keep one")
	require.NotContains(t, err.Error(), home)
}

func yamlJSON(t *testing.T, body string) string {
	t.Helper()
	f, err := load(t, "a.yaml", body)
	require.NoError(t, err)
	return string(f.Data)
}

func TestLoadKeepsScalarText(t *testing.T) {
	require.JSONEq(t, `{"a":"2024-01-01"}`, yamlJSON(t, "a: 2024-01-01\n"))
	require.JSONEq(t, `{"a":"2024-01-01T10:00:00Z"}`, yamlJSON(t, "a: 2024-01-01T10:00:00Z\n"))
	require.Equal(t, `{"a":1.0,"b":0.50}`, yamlJSON(t, "a: 1.0\nb: 0.50\n"))
	require.JSONEq(t, `{"a":31,"b":true,"c":null,"d":"8080"}`, yamlJSON(t, "a: 0x1F\nb: true\nc: ~\nd: \"8080\"\n"))
}

func TestLoadInfAndNaNError(t *testing.T) {
	for _, v := range []string{".inf", "-.inf", ".nan"} {
		require.ErrorContains(t, yamlErr(t, "x:\n  y: "+v+"\n"), "x.y")
	}
}

func TestLoadMergeKey(t *testing.T) {
	body := "base: &b\n  a: 1\n  b: 2\nuse:\n  <<: *b\n  b: 3\nmulti:\n  <<: [*b, {c: 4}]\n"
	require.JSONEq(t, `{"base":{"a":1,"b":2},"use":{"a":1,"b":3},"multi":{"a":1,"b":2,"c":4}}`, yamlJSON(t, body))
}

func TestLoadAliasValue(t *testing.T) {
	require.JSONEq(t, `{"a":[1,2],"b":[1,2]}`, yamlJSON(t, "a: &x [1, 2]\nb: *x\n"))
}

func TestLoadBinaryAndNullDocument(t *testing.T) {
	require.ErrorContains(t, yamlErr(t, "a: !!binary aGVsbG8=\n"), "a: tag !!binary is not supported")
	require.JSONEq(t, `{}`, yamlJSON(t, "~\n"))
}

func TestLenientNullIsEmpty(t *testing.T) {
	l, err := StringList("args", []byte("null"))
	require.NoError(t, err)
	require.Nil(t, l)
	m, err := StringMap("env", []byte("null"))
	require.NoError(t, err)
	require.Nil(t, m)

	m, err = StringMap("env", []byte(`{"PORT":8080,"DEBUG":true,"F":1.0}`))
	require.NoError(t, err)
	require.Equal(t, map[string]string{"PORT": "8080", "DEBUG": "true", "F": "1.0"}, m)
	_, err = StringMap("env", []byte(`{"A":[1]}`))
	require.ErrorContains(t, err, "env.A")
}
