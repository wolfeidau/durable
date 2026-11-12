package skills

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// FilesFunc returns a file system for one operation, bound to ctx because fs.FS takes none.
type FilesFunc func(ctx context.Context) fs.FS

// rootFS reads inside an os.Root. Unlike Root.FS it opens with O_NONBLOCK, so a FIFO can't hang the open.
type rootFS struct{ r *os.Root }

var _ fs.FS = rootFS{}

func (f rootFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	file, err := f.r.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return file, nil
}

// openLocal returns the host directory root as a file system, and the function that closes it.
func openLocal(root string) (fs.FS, func(), error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open root: %w", err)
	}
	return rootFS{r}, func() { _ = r.Close() }, nil
}

// readFS reads rel (a slash path) through one handle. It reports whether the file was cut at maxFileBytes.
func readFS(fsys fs.FS, rel string) ([]byte, bool, error) {
	f, err := fsys.Open(rel)
	if err != nil {
		return nil, false, fmt.Errorf("failed to open %s: %w", rel, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("failed to stat %s: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s is not a regular file", rel)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("failed to read %s: %w", rel, err)
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return nil, false, fmt.Errorf("%s is a binary file", rel)
	}
	if len(b) > maxFileBytes {
		return b[:maxFileBytes], true, nil
	}
	return b, false, nil
}

// readConfined reads rel inside the host directory root, so a symlink or swap can't leave root.
func readConfined(root, rel string) ([]byte, bool, error) {
	fsys, closeFS, err := openLocal(root)
	if err != nil {
		return nil, false, err
	}
	defer closeFS()
	return readFS(fsys, filepath.ToSlash(rel))
}
