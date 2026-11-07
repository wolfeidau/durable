package server

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

func readUI(t *testing.T, fsys fs.FS) string {
	t.Helper()
	b, err := fs.ReadFile(uiRoot(fsys), "index.html")
	require.NoError(t, err)
	return string(b)
}

func TestUIRootFallsBackToPlaceholder(t *testing.T) {
	fsys := fstest.MapFS{
		"ui/placeholder/index.html": {Data: []byte("placeholder")},
		"ui/dist/app.js":            {Data: []byte("js")},
	}
	require.Equal(t, "placeholder", readUI(t, fsys), "a dist without index.html is not a built UI")

	fsys["ui/dist/index.html"] = &fstest.MapFile{Data: []byte("built")}
	require.Equal(t, "built", readUI(t, fsys))
}

func TestEmbeddedUIHasIndex(t *testing.T) {
	require.Contains(t, readUI(t, uiFS), "<title>durable</title>")
}
