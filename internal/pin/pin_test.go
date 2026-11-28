package pin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

type sample struct {
	Name string `json:"name"`
}

func TestWriteOnceFirstWriterWinsAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.pin")

	won, err := WriteOnce(path, []byte("first"))
	require.NoError(t, err)
	require.True(t, won)

	won, err = WriteOnce(path, []byte("second"))
	require.NoError(t, err)
	require.False(t, won)

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "first", string(b))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestReadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.json")

	got, ok, err := ReadJSON[sample](path, "sample")
	require.NoError(t, err)
	require.False(t, ok)
	require.Zero(t, got)

	require.NoError(t, os.WriteFile(path, []byte(`{"name":"ok"}`), 0o600))
	got, ok, err = ReadJSON[sample](path, "sample")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, sample{Name: "ok"}, got)

	require.NoError(t, os.WriteFile(path, []byte(`{bad`), 0o600))
	_, ok, err = ReadJSON[sample](path, "sample")
	require.ErrorContains(t, err, "failed to parse pinned sample")
	require.False(t, ok)
}

func TestEnsureJSON(t *testing.T) {
	t.Run("fresh discovery pins value and reuses it", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sample.json")
		calls := 0
		got, pinned, err := EnsureJSON(path, "sample", false, sample{}, func() (sample, error) {
			calls++
			return sample{Name: "fresh"}, nil
		})
		require.NoError(t, err)
		require.False(t, pinned)
		require.Equal(t, sample{Name: "fresh"}, got)
		require.Equal(t, 1, calls)

		got, pinned, err = EnsureJSON(path, "sample", false, sample{}, func() (sample, error) {
			calls++
			return sample{Name: "other"}, nil
		})
		require.NoError(t, err)
		require.True(t, pinned)
		require.Equal(t, sample{Name: "fresh"}, got)
		require.Equal(t, 1, calls)
	})

	t.Run("existed pins empty without discovery", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sample.json")
		called := false
		got, pinned, err := EnsureJSON(path, "sample", true, sample{Name: "legacy"}, func() (sample, error) {
			called = true
			return sample{Name: "fresh"}, nil
		})
		require.NoError(t, err)
		require.True(t, pinned)
		require.False(t, called)
		require.Equal(t, sample{Name: "legacy"}, got)
	})

	t.Run("lost race returns winner", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sample.json")
		got, pinned, err := EnsureJSON(path, "sample", false, sample{}, func() (sample, error) {
			won, err := WriteOnce(path, []byte(`{"name":"winner"}`))
			require.NoError(t, err)
			require.True(t, won)
			return sample{Name: "loser"}, nil
		})
		require.NoError(t, err)
		require.True(t, pinned)
		require.Equal(t, sample{Name: "winner"}, got)
	})
}
