package worker

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	workerv1 "github.com/wolfeidau/durable/api/durable/worker/v1"
	"github.com/wolfeidau/durable/api/durable/worker/v1/workerv1connect"
)

func readFile(t *testing.T, c workerv1connect.ToolServiceClient, path string, maxBytes int64) (*workerv1.ReadFileResponse, error) {
	t.Helper()
	res, err := c.ReadFile(t.Context(), authed(&workerv1.ReadFileRequest{Path: path, MaxBytes: maxBytes}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func requireCode(t *testing.T, want connect.Code, err error) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, want, connect.CodeOf(err), err.Error())
}

func TestReadFileReturnsARegularFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "f.txt"), []byte("hello"), 0o600))
	c := startRoot(t, root)

	res, err := readFile(t, c, "a/f.txt", 100)

	require.NoError(t, err)
	require.Equal(t, "hello", string(res.Data))
	require.False(t, res.Truncated)
}

func TestReadFileCapsAndReportsTruncation(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "f"), []byte("0123456789"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "big"), bytes.Repeat([]byte("a"), maxReadBytes+10), 0o600))
	c := startRoot(t, root)

	res, err := readFile(t, c, "f", 4)
	require.NoError(t, err)
	require.Equal(t, "0123", string(res.Data))
	require.True(t, res.Truncated)

	res, err = readFile(t, c, "f", 10)
	require.NoError(t, err)
	require.Len(t, res.Data, 10)
	require.False(t, res.Truncated)

	for _, limit := range []int64{0, maxReadBytes * 4} {
		res, err = readFile(t, c, "big", limit)
		require.NoError(t, err)
		require.Len(t, res.Data, maxReadBytes)
		require.True(t, res.Truncated)
	}
}

func TestReadFileRefusesASymlinkOutOfTheRoot(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "link")))
	require.NoError(t, os.Symlink("../"+filepath.Base(outside)+"/secret", filepath.Join(root, "rel")))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "dir")))
	c := startRoot(t, root)

	for _, p := range []string{"link", "rel", "dir/secret"} {
		_, err := readFile(t, c, p, 10)
		requireCode(t, connect.CodeInvalidArgument, err)
	}
}

func TestReadFileFollowsASymlinkInsideTheRoot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "real"), []byte("in"), 0o600))
	require.NoError(t, os.Symlink("real", filepath.Join(root, "link")))
	c := startRoot(t, root)

	res, err := readFile(t, c, "link", 10)

	require.NoError(t, err)
	require.Equal(t, "in", string(res.Data))
}

func TestReadFileRefusesAFIFOWithoutHanging(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, syscall.Mkfifo(filepath.Join(root, "fifo"), 0o600))
	c := startRoot(t, root)

	done := make(chan error, 1)
	go func() { _, err := readFile(t, c, "fifo", 10); done <- err }()

	select {
	case err := <-done:
		requireCode(t, connect.CodeFailedPrecondition, err)
	case <-time.After(10 * time.Second):
		t.Fatal("ReadFile hung on a FIFO")
	}
}

func TestReadFileRefusesNonLocalPaths(t *testing.T) {
	root := t.TempDir()
	c := startRoot(t, root)

	for _, p := range []string{"", "/etc/passwd", "../x", "a/../../x"} {
		_, err := readFile(t, c, p, 10)
		requireCode(t, connect.CodeInvalidArgument, err)
	}
}

func TestReadFileMissingIsNotFound(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "f"), nil, 0o600))
	c := startRoot(t, root)

	for _, p := range []string{"nope", "f/x", "nope/x"} {
		_, err := readFile(t, c, p, 10)
		requireCode(t, connect.CodeNotFound, err)
	}
}

func TestReadFileRefusesADirectory(t *testing.T) {
	root := t.TempDir()
	c := startRoot(t, root)

	_, err := readFile(t, c, ".", 10)

	requireCode(t, connect.CodeFailedPrecondition, err)
}

func TestReadFileNeedsTheToken(t *testing.T) {
	c := startRoot(t, t.TempDir())

	_, err := c.ReadFile(t.Context(), connect.NewRequest(&workerv1.ReadFileRequest{Path: "f"}))

	requireCode(t, connect.CodeUnauthenticated, err)
}

func readDir(t *testing.T, c workerv1connect.ToolServiceClient, path string) (*workerv1.ReadDirResponse, error) {
	t.Helper()
	res, err := c.ReadDir(t.Context(), authed(&workerv1.ReadDirRequest{Path: path}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func TestReadDirListsEntriesWithTheirOwnTypes(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "sub"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.txt"), nil, 0o600))
	require.NoError(t, os.Symlink("b.txt", filepath.Join(root, "a-link")))
	c := startRoot(t, root)

	res, err := readDir(t, c, ".")

	require.NoError(t, err)
	require.False(t, res.Truncated)
	require.Len(t, res.Entries, 3)
	require.Equal(t, "a-link", res.Entries[0].Name)
	require.NotZero(t, fs.FileMode(res.Entries[0].Mode)&fs.ModeSymlink)
	require.Equal(t, "b.txt", res.Entries[1].Name)
	require.True(t, fs.FileMode(res.Entries[1].Mode).IsRegular())
	require.Equal(t, "sub", res.Entries[2].Name)
	require.True(t, fs.FileMode(res.Entries[2].Mode).IsDir())
}

func TestReadDirCapsEntries(t *testing.T) {
	root := t.TempDir()
	for i := range maxDirEntries + 5 {
		require.NoError(t, os.WriteFile(filepath.Join(root, "f"+strconv.Itoa(i)), nil, 0o600))
	}
	c := startRoot(t, root)

	res, err := readDir(t, c, ".")

	require.NoError(t, err)
	require.Len(t, res.Entries, maxDirEntries)
	require.True(t, res.Truncated)
}

func TestReadDirIsConfined(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "out")))
	require.NoError(t, os.WriteFile(filepath.Join(root, "f"), nil, 0o600))
	c := startRoot(t, root)

	_, err := readDir(t, c, "out")
	requireCode(t, connect.CodeInvalidArgument, err)
	_, err = readDir(t, c, "../")
	requireCode(t, connect.CodeInvalidArgument, err)
	_, err = readDir(t, c, "/")
	requireCode(t, connect.CodeInvalidArgument, err)
	_, err = readDir(t, c, "nope")
	requireCode(t, connect.CodeNotFound, err)
	_, err = readDir(t, c, "f")
	requireCode(t, connect.CodeFailedPrecondition, err)
}

func TestStatDescribesFilesAndDirectories(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "d"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "d", "f"), []byte("abc"), 0o600))
	require.NoError(t, os.Symlink("d/f", filepath.Join(root, "in")))
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(root, "out")))
	c := startRoot(t, root)
	stat := func(p string) (*workerv1.StatResponse, error) {
		res, err := c.Stat(t.Context(), authed(&workerv1.StatRequest{Path: p}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}

	res, err := stat("d/f")
	require.NoError(t, err)
	require.Equal(t, "f", res.Name)
	require.EqualValues(t, 3, res.Size)
	require.True(t, fs.FileMode(res.Mode).IsRegular())

	res, err = stat(".")
	require.NoError(t, err)
	require.Equal(t, ".", res.Name)
	require.True(t, fs.FileMode(res.Mode).IsDir())

	res, err = stat("in")
	require.NoError(t, err)
	require.EqualValues(t, 3, res.Size)

	_, err = stat("out")
	requireCode(t, connect.CodeInvalidArgument, err)
	_, err = stat("nope")
	requireCode(t, connect.CodeNotFound, err)
	_, err = stat("../x")
	requireCode(t, connect.CodeInvalidArgument, err)
}
