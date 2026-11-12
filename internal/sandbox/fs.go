package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"time"

	"connectrpc.com/connect"

	workerv1 "github.com/wolfeidau/durable/api/durable/worker/v1"
)

// maxFileRead is what the worker returns at most for one file: 1 MiB, plus one byte so a reader can tell a cut file.
const maxFileRead = 1<<20 + 1

// ErrWorker marks a failure of the worker itself, as opposed to something about the file asked for.
var ErrWorker = errors.New("worker failed")

var errOldWorker = workerError{errors.New("the worker image is older than the server; rebuild it")}

type workerError struct{ err error }

func (e workerError) Error() string      { return e.err.Error() }
func (e workerError) Unwrap() error      { return e.err }
func (workerError) Is(target error) bool { return target == ErrWorker }

// Root returns the workspace root the worker's tools are confined to.
func (c *Client) Root(ctx context.Context) (string, error) {
	res, err := c.rpc.Info(ctx, connect.NewRequest(&workerv1.InfoRequest{}))
	if err != nil {
		return "", fmt.Errorf("failed to get worker info: %w", err)
	}
	return res.Msg.Root, nil
}

// FS returns the worker's workspace as a read-only file system. Its reads use ctx, because fs.FS takes none.
func (c *Client) FS(ctx context.Context) fs.FS { return &workerFS{ctx: ctx, c: c} }

//nolint:containedctx // fs.FS has no context parameter, so each FS lives only as long as one operation's context
type workerFS struct {
	ctx context.Context
	c   *Client
}

var (
	_ fs.ReadDirFS  = (*workerFS)(nil)
	_ fs.StatFS     = (*workerFS)(nil)
	_ fs.ReadFileFS = (*workerFS)(nil)
)

// pathError maps a worker error to the io/fs sentinels callers test for.
func pathError(op, name string, err error) error {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return &fs.PathError{Op: op, Path: name, Err: workerError{err}}
	}
	var inner error
	switch ce.Code() {
	case connect.CodeNotFound:
		inner = fs.ErrNotExist
	case connect.CodeInvalidArgument:
		inner = fmt.Errorf("%w (%s)", fs.ErrInvalid, ce.Message())
	case connect.CodePermissionDenied:
		inner = fs.ErrPermission
	case connect.CodeFailedPrecondition:
		inner = errors.New(ce.Message())
	case connect.CodeUnimplemented:
		inner = errOldWorker
	default:
		inner = workerError{err}
	}
	return &fs.PathError{Op: op, Path: name, Err: inner}
}

func invalid(op, name string) error {
	return &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
}

func (w *workerFS) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, invalid("stat", name)
	}
	res, err := w.c.rpc.Stat(w.ctx, connect.NewRequest(&workerv1.StatRequest{Path: name}))
	if err != nil {
		return nil, pathError("stat", name, err)
	}
	return fileInfo{name: res.Msg.Name, mode: fs.FileMode(res.Msg.Mode), size: res.Msg.Size}, nil
}

// ReadDir drops the worker's truncated flag: its 1024-entry cap is above the cap of every caller, which are skills and their resources.
func (w *workerFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, invalid("readdir", name)
	}
	res, err := w.c.rpc.ReadDir(w.ctx, connect.NewRequest(&workerv1.ReadDirRequest{Path: name}))
	if err != nil {
		return nil, pathError("readdir", name, err)
	}
	out := make([]fs.DirEntry, 0, len(res.Msg.Entries))
	for _, e := range res.Msg.Entries {
		out = append(out, dirEntry{w: w, dir: name, name: e.Name, mode: fs.FileMode(e.Mode)})
	}
	return out, nil
}

func (w *workerFS) ReadFile(name string) ([]byte, error) {
	if !fs.ValidPath(name) {
		return nil, invalid("readfile", name)
	}
	res, err := w.c.rpc.ReadFile(w.ctx, connect.NewRequest(&workerv1.ReadFileRequest{Path: name, MaxBytes: maxFileRead}))
	if err != nil {
		return nil, pathError("readfile", name, err)
	}
	return res.Msg.Data, nil
}

// Open reads a whole file, up to the worker's cap, into memory; a directory is listed on its first ReadDir.
func (w *workerFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, invalid("open", name)
	}
	info, err := w.Stat(name)
	if err != nil {
		return nil, relabel("open", name, err)
	}
	if info.IsDir() {
		return &dirFile{w: w, name: name, info: info}, nil
	}
	data, err := w.ReadFile(name)
	if err != nil {
		return nil, relabel("open", name, err)
	}
	return &memFile{Reader: bytes.NewReader(data), info: info}, nil
}

// relabel re-labels an error from Stat or ReadFile with the operation the caller asked for.
func relabel(op, name string, err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return &fs.PathError{Op: op, Path: name, Err: pe.Err}
	}
	return &fs.PathError{Op: op, Path: name, Err: err}
}

type fileInfo struct {
	name string
	mode fs.FileMode
	size int64
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.size }
func (i fileInfo) Mode() fs.FileMode  { return i.mode }
func (i fileInfo) ModTime() time.Time { return time.Time{} }
func (i fileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fileInfo) Sys() any           { return nil }

type dirEntry struct {
	w    *workerFS
	dir  string
	name string
	mode fs.FileMode // the entry's type bits only
}

func (e dirEntry) Name() string      { return e.name }
func (e dirEntry) IsDir() bool       { return e.mode.IsDir() }
func (e dirEntry) Type() fs.FileMode { return e.mode.Type() }

func (e dirEntry) Info() (fs.FileInfo, error) {
	// Stat follows symlinks, so a link reports only its own type, as os.ReadDir's entry would.
	if e.mode&fs.ModeSymlink != 0 {
		return fileInfo{name: e.name, mode: e.mode}, nil
	}
	return e.w.Stat(path.Join(e.dir, e.name))
}

type memFile struct {
	*bytes.Reader
	info fs.FileInfo
}

func (f *memFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *memFile) Close() error               { return nil }

type dirFile struct {
	w       *workerFS
	name    string
	info    fs.FileInfo
	entries []fs.DirEntry
	loaded  bool
	next    int
}

func (d *dirFile) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *dirFile) Close() error               { return nil }

func (d *dirFile) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: errors.New("is a directory")}
}

func (d *dirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if !d.loaded {
		entries, err := d.w.ReadDir(d.name)
		if err != nil {
			return nil, err
		}
		d.entries, d.loaded = entries, true
	}
	rest := d.entries[d.next:]
	if n <= 0 {
		d.next = len(d.entries)
		return rest, nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	n = min(n, len(rest))
	d.next += n
	return rest[:n], nil
}
