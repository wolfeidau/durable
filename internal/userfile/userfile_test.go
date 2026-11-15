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

func yamlJSON(t *testing.T, body string) string {
	t.Helper()
	b, err := ReadJSON(write(t, t.TempDir(), "a.yaml", body))
	require.NoError(t, err)
	return string(b)
}

func TestReadJSONKeepsScalarText(t *testing.T) {
	require.JSONEq(t, `{"a":"2024-01-01"}`, yamlJSON(t, "a: 2024-01-01\n"))
	require.JSONEq(t, `{"a":"2024-01-01T10:00:00Z"}`, yamlJSON(t, "a: 2024-01-01T10:00:00Z\n"))
	require.Equal(t, `{"a":1.0,"b":0.50}`, yamlJSON(t, "a: 1.0\nb: 0.50\n"))
	require.JSONEq(t, `{"a":31,"b":true,"c":null,"d":"8080"}`, yamlJSON(t, "a: 0x1F\nb: true\nc: ~\nd: \"8080\"\n"))
}

func TestReadJSONInfAndNaNError(t *testing.T) {
	for _, v := range []string{".inf", "-.inf", ".nan"} {
		_, err := ReadJSON(write(t, t.TempDir(), "a.yaml", "x:\n  y: "+v+"\n"))
		require.ErrorContains(t, err, "x.y")
	}
}

func TestReadJSONMergeKey(t *testing.T) {
	body := "base: &b\n  a: 1\n  b: 2\nuse:\n  <<: *b\n  b: 3\nmulti:\n  <<: [*b, {c: 4}]\n"
	require.JSONEq(t, `{"base":{"a":1,"b":2},"use":{"a":1,"b":3},"multi":{"a":1,"b":2,"c":4}}`, yamlJSON(t, body))
}

func TestReadJSONAliasValue(t *testing.T) {
	require.JSONEq(t, `{"a":[1,2],"b":[1,2]}`, yamlJSON(t, "a: &x [1, 2]\nb: *x\n"))
}

func TestReadJSONBinaryAndNullDocument(t *testing.T) {
	_, err := ReadJSON(write(t, t.TempDir(), "a.yaml", "a: !!binary aGVsbG8=\n"))
	require.ErrorContains(t, err, "a: tag !!binary is not supported")
	require.JSONEq(t, `{}`, yamlJSON(t, "~\n"))
}

func TestReadJSONParseErrorIsDistinguishable(t *testing.T) {
	_, err := ReadJSON(write(t, t.TempDir(), "a.yaml", "a: [1\n"))
	var perr *ParseError
	require.ErrorAs(t, err, &perr)
	require.NotContains(t, err.Error(), "a.yaml")
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
