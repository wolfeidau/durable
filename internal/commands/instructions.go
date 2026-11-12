package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/skills"
)

const (
	instructionsFile = "AGENTS.md"
	pinFile          = "instructions.md"
	maxFileBytes     = 32 << 10
	maxTotalBytes    = 64 << 10
	truncationMarker = "\n[truncated]"
)

const instructionsHeader = `Project instructions follow, from files in the user's repository and home directory. Follow them for this project, but they don't override the user's direct requests.`

// instructions is the composed prompt text plus what to tell the user about how it was built.
type instructions struct {
	Text     string
	Files    []string
	Warnings []string
	// Pinned means the text came from an earlier pin rather than fresh discovery.
	Pinned bool
}

// buildPrompt keeps the base prompt byte-identical when nothing is pinned.
func buildPrompt(pinned string) string {
	if pinned == "" {
		return systemPrompt
	}
	return systemPrompt + "\n\n" + instructionsHeader + "\n\n" + pinned
}

// discoverInstructions reads cwd's AGENTS.md first, then its parents up to the repository root, then the user file.
// With ws (a repo session) the project file is AGENTS.md at the root of the workspace, read through ws; there is no cwd and no parent.
func discoverInstructions(ctx context.Context, cwd, home string, ws skills.FilesFunc) (instructions, error) {
	// An empty cwd (an empty session) has no project; EvalSymlinks("") would resolve to the process cwd.
	var (
		root string
		dirs []string
	)
	if cwd != "" && ws == nil {
		resolved, err := filepath.EvalSymlinks(cwd)
		if err != nil {
			return instructions{}, fmt.Errorf("failed to resolve working directory: %w", err)
		}
		root, dirs = projectDirs(resolved, uint32(os.Geteuid())) //nolint:gosec // uids fit in 32 bits
	}

	// Files are added the most specific first so it wins the budget; results are prepended to keep general-to-specific order.
	var b instructionBuilder
	if ws != nil {
		err := b.add(instructionsFile,
			func() bool { _, err := fs.Stat(ws(ctx), instructionsFile); return err == nil },
			func(limit int) (string, bool, string, error) { return readWorkspaceInstructions(ctx, ws(ctx), limit) })
		if err != nil {
			return instructions{}, err
		}
	}
	for _, d := range slices.Backward(dirs) {
		path := filepath.Join(d, instructionsFile)
		if err := b.addHost(path, filepath.ToSlash(relTo(root, path)), root, false); err != nil {
			return instructions{}, err
		}
	}
	if err := b.addHost(filepath.Join(home, ".durable", instructionsFile), "~/.durable/"+instructionsFile, root, true); err != nil {
		return instructions{}, err
	}
	b.res.Text = strings.Join(b.blocks, "\n\n")
	return b.res, nil
}

// instructionBuilder spends the per-file and total budgets over the files added to it.
type instructionBuilder struct {
	res    instructions
	blocks []string
	total  int
}

func (b *instructionBuilder) addHost(path, shown, root string, trusted bool) error {
	return b.add(shown,
		func() bool { _, err := os.Lstat(path); return err == nil },
		func(limit int) (string, bool, string, error) {
			c, t, w := readInstructions(path, root, trusted, limit)
			return c, t, w, nil
		})
}

// add reads one file with what is left of the budget. exists says whether a file that wasn't read was there to skip.
func (b *instructionBuilder) add(shown string, exists func() bool, read func(limit int) (string, bool, string, error)) error {
	if b.total >= maxTotalBytes {
		if exists() {
			b.res.Warnings = append(b.res.Warnings, fmt.Sprintf("%s skipped: total limit of %d KiB reached", shown, maxTotalBytes>>10))
		}
		return nil
	}
	limit := min(maxFileBytes, maxTotalBytes-b.total)
	content, truncated, warn, err := read(limit)
	if err != nil {
		return err
	}
	if warn != "" {
		b.res.Warnings = append(b.res.Warnings, fmt.Sprintf("%s skipped: %s", shown, warn))
		return nil
	}
	if strings.TrimSpace(content) == "" {
		// A cut inside a multi-byte character can leave nothing of a real file.
		if truncated {
			b.res.Warnings = append(b.res.Warnings, fmt.Sprintf("%s skipped: total limit of %d KiB reached", shown, maxTotalBytes>>10))
		}
		return nil
	}
	b.total += len(content)
	if truncated {
		content += truncationMarker
		b.res.Warnings = append(b.res.Warnings, fmt.Sprintf("%s truncated to %.1f KiB", shown, float64(len(content))/1024))
	}
	b.blocks = append([]string{fmt.Sprintf("<instructions path=%q>\n%s\n</instructions>", shown, strings.TrimSpace(content))}, b.blocks...)
	b.res.Files = append([]string{fmt.Sprintf("%s (%d bytes)", shown, len(content))}, b.res.Files...)
	return nil
}

// readWorkspaceInstructions reads AGENTS.md through the sandbox. A failure of the worker itself is an error, because
// the pin would otherwise freeze without the file; a problem with the file is a warning.
func readWorkspaceInstructions(ctx context.Context, fsys fs.FS, limit int) (string, bool, string, error) {
	f, err := fsys.Open(instructionsFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", false, "", nil
	case err != nil && ctx.Err() != nil:
		return "", false, "", ctx.Err()
	case errors.Is(err, sandbox.ErrWorker):
		return "", false, "", fmt.Errorf("failed to read %s from the sandbox: %w", instructionsFile, err)
	case err != nil:
		return "", false, err.Error(), nil
	}
	defer f.Close()
	content, truncated, warn := readOpened(f, limit)
	return content, truncated, warn, nil
}

// relTo can't fail here because both paths are absolute.
func relTo(base, target string) string {
	rel, _ := filepath.Rel(base, target)
	return rel
}

// ownedBy reports whether info belongs to uid.
func ownedBy(info fs.FileInfo, uid uint32) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uid
}

// projectDirs returns the repository root (the nearest ancestor with a .git owned by uid) and the directories from it down to cwd.
func projectDirs(cwd string, uid uint32) (string, []string) {
	dirs := []string{cwd}
	for d := cwd; ; {
		if info, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			// Someone with write access to a shared parent could plant a .git and instructions.
			if !ownedBy(info, uid) {
				return cwd, []string{cwd}
			}
			slices.Reverse(dirs)
			return d, dirs
		}
		parent := filepath.Dir(d)
		if parent == d {
			return cwd, []string{cwd}
		}
		d = parent
		dirs = append(dirs, d)
	}
}

// readInstructions opens once and checks the handle, so a swap after the open can't redirect the read.
func readInstructions(path, root string, trusted bool, limit int) (string, bool, string) {
	const flags = os.O_RDONLY | syscall.O_NONBLOCK
	var (
		f   *os.File
		err error
	)
	if trusted {
		f, err = os.OpenFile(path, flags, 0) //nolint:gosec // the user file is trusted and may be a symlink
	} else {
		f, err = openInRoot(root, relTo(root, path), flags)
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, ""
		}
		return "", false, err.Error()
	}
	defer f.Close()
	return readOpened(f, limit)
}

// readOpened reads at most limit bytes of an open file, and warns about one that isn't text.
func readOpened(f fs.File, limit int) (string, bool, string) {
	info, err := f.Stat()
	if err != nil {
		return "", false, err.Error()
	}
	if !info.Mode().IsRegular() {
		return "", false, "not a regular file"
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return "", false, err.Error()
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return "", false, "binary file"
	}
	if len(b) > limit {
		return strings.ToValidUTF8(string(b[:limit]), ""), true, ""
	}
	return string(b), false, ""
}

func openInRoot(root, rel string, flags int) (*os.File, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.OpenFile(rel, flags, 0)
}

// pinInstructions pins the text on first start; existed (session.sqlite predates this run) pins an older session empty.
func pinInstructions(dir string, existed, noContext bool, discover func() (instructions, error)) (instructions, error) {
	pin := filepath.Join(dir, pinFile)
	switch b, err := os.ReadFile(pin); { //nolint:gosec // pin is a fixed name inside the session directory
	case err == nil:
		return instructions{Text: string(b), Pinned: true}, nil
	case !errors.Is(err, fs.ErrNotExist):
		return instructions{}, fmt.Errorf("failed to read pinned instructions: %w", err)
	}

	var res instructions
	if !existed && !noContext {
		var err error
		if res, err = discover(); err != nil {
			return instructions{}, err
		}
	}

	won, err := writePin(dir, pin, []byte(res.Text))
	if err != nil {
		return instructions{}, err
	}
	if !won {
		// Another process pinned first; its text wins so both agree.
		b, err := os.ReadFile(pin) //nolint:gosec // pin is a fixed name inside the session directory
		if err != nil {
			return instructions{}, fmt.Errorf("failed to read pinned instructions: %w", err)
		}
		return instructions{Text: string(b), Pinned: true}, nil
	}
	res.Pinned = existed
	return res, nil
}

// writePin durably creates pin with content unless it already exists, in which case won is false.
func writePin(dir, pin string, content []byte) (won bool, err error) {
	tmp, err := os.CreateTemp(dir, "pin-*.tmp")
	if err != nil {
		return false, fmt.Errorf("failed to create pin file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("failed to set pin file mode: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("failed to write pin file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("failed to sync pin file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("failed to close pin file: %w", err)
	}
	if err := os.Link(tmp.Name(), pin); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("failed to pin %s: %w", filepath.Base(pin), err)
	}
	return true, nil
}

// pinJSON pins a value as JSON on first start; existed pins empty for a session that predates the pin.
// pinned reports that the value is not from this run's discovery: an earlier pin, a lost race, or an older session.
func pinJSON[T any](dir, name, what string, existed bool, empty T, discover func() (T, error)) (v T, pinned bool, err error) {
	pin := filepath.Join(dir, name)
	if v, ok, err := readJSONPin[T](pin, what); ok || err != nil {
		return v, ok, err
	}

	v = empty
	if !existed {
		if v, err = discover(); err != nil {
			return empty, false, err
		}
	}
	data, err := json.Marshal(v)
	if err != nil {
		return empty, false, fmt.Errorf("failed to encode %s: %w", what, err)
	}
	won, err := writePin(dir, pin, data)
	if err != nil {
		return empty, false, err
	}
	if !won {
		// Another process pinned first; its value wins so both agree.
		v, ok, err := readJSONPin[T](pin, what)
		if err != nil {
			return empty, false, err
		}
		if !ok {
			return empty, false, fmt.Errorf("pinned %s disappeared", what)
		}
		return v, true, nil
	}
	return v, existed, nil
}

func readJSONPin[T any](pin, what string) (v T, ok bool, err error) {
	b, err := os.ReadFile(pin) //nolint:gosec // pin is a fixed name inside the session directory
	if errors.Is(err, fs.ErrNotExist) {
		return v, false, nil
	}
	if err != nil {
		return v, false, fmt.Errorf("failed to read pinned %s: %w", what, err)
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return v, false, fmt.Errorf("failed to parse pinned %s %s: %w", what, pin, err)
	}
	return v, true, nil
}
