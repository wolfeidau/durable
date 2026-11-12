package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"connectrpc.com/connect"

	workerv1 "github.com/wolfeidau/durable/api/durable/worker/v1"
)

const (
	// maxReadBytes is one byte over the 1 MiB the server's readers keep, so they can tell a cut file from an exact fit.
	maxReadBytes  = 1<<20 + 1
	maxDirEntries = 1024
	// A FIFO must not hang the open.
	nonblockOpen = os.O_RDONLY | syscall.O_NONBLOCK
)

// openRoot checks path and opens the workspace root; every read goes through the root, so a symlink can't leave it.
func (s *service) openRoot(path string) (*os.Root, error) {
	if !filepath.IsLocal(path) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%q must be relative and stay inside the workspace", path))
	}
	root, err := os.OpenRoot(s.cfg.Root)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to open workspace root: %w", err))
	}
	return root, nil
}

func (s *service) open(path string) (*os.File, fs.FileInfo, error) {
	root, err := s.openRoot(path)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	f, err := root.OpenFile(path, nonblockOpen, 0)
	if err != nil {
		return nil, nil, fsError(path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, fsError(path, err)
	}
	return f, info, nil
}

func fsError(path string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s: not found", path))
	case errors.Is(err, fs.ErrPermission):
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s: permission denied", path))
	}
	// A symlink leaving the root is refused with an error of no fixed type.
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s: %w", path, err))
}

func (s *service) ReadFile(_ context.Context, req *connect.Request[workerv1.ReadFileRequest]) (*connect.Response[workerv1.ReadFileResponse], error) {
	m := req.Msg
	f, info, err := s.open(m.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if !info.Mode().IsRegular() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s is not a regular file", m.Path))
	}
	limit := m.MaxBytes
	if limit <= 0 || limit > maxReadBytes {
		limit = maxReadBytes
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to read %s: %w", m.Path, err))
	}
	truncated := int64(len(b)) > limit
	if truncated {
		b = b[:limit]
	}
	return connect.NewResponse(&workerv1.ReadFileResponse{Data: b, Truncated: truncated}), nil
}

func (s *service) ReadDir(_ context.Context, req *connect.Request[workerv1.ReadDirRequest]) (*connect.Response[workerv1.ReadDirResponse], error) {
	m := req.Msg
	f, info, err := s.open(m.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if !info.IsDir() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s is not a directory", m.Path))
	}
	entries, err := f.ReadDir(maxDirEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to read %s: %w", m.Path, err))
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	truncated := len(entries) > maxDirEntries
	if truncated {
		entries = entries[:maxDirEntries]
	}
	out := make([]*workerv1.DirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, &workerv1.DirEntry{Name: e.Name(), Mode: uint32(e.Type())})
	}
	return connect.NewResponse(&workerv1.ReadDirResponse{Entries: out, Truncated: truncated}), nil
}

func (s *service) Stat(_ context.Context, req *connect.Request[workerv1.StatRequest]) (*connect.Response[workerv1.StatResponse], error) {
	m := req.Msg
	root, err := s.openRoot(m.Path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Stat(m.Path)
	if err != nil {
		return nil, fsError(m.Path, err)
	}
	return connect.NewResponse(&workerv1.StatResponse{Name: filepath.Base(m.Path), Mode: uint32(info.Mode()), Size: info.Size()}), nil
}
