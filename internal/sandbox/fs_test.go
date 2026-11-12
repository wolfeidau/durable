package sandbox_test

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/api/durable/worker/v1/workerv1connect"
	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/sandbox/sandboxtest"
)

func fsFake(t *testing.T) (fs.FS, string) {
	t.Helper()
	f := sandboxtest.New(t)
	c, closer, err := f.Connect(t.Context(), "n", "tok")
	require.NoError(t, err)
	t.Cleanup(func() { _ = closer.Close() })
	root := f.Root("n")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "top.txt"), []byte("top"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "one.txt"), []byte("one"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "b", "two.txt"), []byte("two"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "empty"), nil, 0o600))
	return c.FS(t.Context()), root
}

func TestClientFSPassesFSTest(t *testing.T) {
	fsys, _ := fsFake(t)

	require.NoError(t, fstest.TestFS(fsys, "top.txt", "a/one.txt", "a/b/two.txt", "empty"))
}

func TestClientFSMapsErrors(t *testing.T) {
	fsys, root := fsFake(t)
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "out")))
	require.NoError(t, os.Symlink("top.txt", filepath.Join(root, "in")))

	_, err := fs.ReadFile(fsys, "nope")
	require.ErrorIs(t, err, fs.ErrNotExist)
	_, err = fsys.Open("../x")
	require.ErrorIs(t, err, fs.ErrInvalid)
	_, err = fsys.Open("/abs")
	require.ErrorIs(t, err, fs.ErrInvalid)
	_, err = fs.ReadFile(fsys, "out/x")
	require.ErrorIs(t, err, fs.ErrInvalid)
	_, err = fs.Stat(fsys, "out")
	require.ErrorIs(t, err, fs.ErrInvalid)

	b, err := fs.ReadFile(fsys, "in")
	require.NoError(t, err)
	require.Equal(t, "top", string(b))

	entries, err := fs.ReadDir(fsys, ".")
	require.NoError(t, err)
	var links []string
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink != 0 {
			links = append(links, e.Name())
		}
	}
	require.Equal(t, []string{"in", "out"}, links)
}

func TestClientFSOpenedFileIsCutOneByteOverTheCap(t *testing.T) {
	fsys, root := fsFake(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "big"), make([]byte, 1<<20+100), 0o600))

	f, err := fsys.Open("big")
	require.NoError(t, err)
	defer f.Close()
	b, err := io.ReadAll(f)

	require.NoError(t, err)
	require.Len(t, b, 1<<20+1)
}

func TestClientFSOpenOfADirectoryIsNotReadable(t *testing.T) {
	fsys, _ := fsFake(t)

	f, err := fsys.Open("a")
	require.NoError(t, err)
	defer f.Close()
	_, err = f.Read(make([]byte, 1))
	require.Error(t, err)
	info, err := f.Stat()
	require.NoError(t, err)
	require.True(t, info.IsDir())
}

func TestClientFSReportsAnOldWorker(t *testing.T) {
	_, h := workerv1connect.NewToolServiceHandler(workerv1connect.UnimplementedToolServiceHandler{})
	srv := httptest.NewUnstartedServer(h)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	c := sandbox.NewClient(srv.URL, "tok", nil)
	t.Cleanup(c.CloseIdle)

	_, err := fs.ReadFile(c.FS(t.Context()), "AGENTS.md")

	require.ErrorContains(t, err, "the worker image is older than the server; rebuild it")
	require.NotErrorIs(t, err, fs.ErrNotExist)
}
