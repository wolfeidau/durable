package commands

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
}

// buildPrompt keeps the base prompt byte-identical when nothing is pinned.
func buildPrompt(pinned string) string {
	if pinned == "" {
		return systemPrompt
	}
	return systemPrompt + "\n\n" + instructionsHeader + "\n\n" + pinned
}

// discoverInstructions reads the user file, then each AGENTS.md from the repository root down to cwd.
func discoverInstructions(cwd, home string) (instructions, error) {
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return instructions{}, fmt.Errorf("failed to resolve working directory: %w", err)
	}
	root, dirs := projectDirs(resolved)

	var (
		res    instructions
		blocks []string
		total  int
	)
	add := func(path, shown string, trusted bool) {
		if total >= maxTotalBytes {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s skipped: total limit of %d KiB reached", shown, maxTotalBytes>>10))
			return
		}
		limit := min(maxFileBytes, maxTotalBytes-total)
		content, truncated, warn := readInstructions(path, root, trusted, limit)
		if warn != "" {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s skipped: %s", shown, warn))
			return
		}
		if strings.TrimSpace(content) == "" {
			return
		}
		total += len(content)
		if truncated {
			content += truncationMarker
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s truncated to %.1f KiB", shown, float64(len(content))/1024))
		}
		blocks = append(blocks, fmt.Sprintf("<instructions path=%q>\n%s\n</instructions>", shown, strings.TrimSpace(content)))
		res.Files = append(res.Files, fmt.Sprintf("%s (%d bytes)", shown, len(content)))
	}

	if home != "" {
		add(filepath.Join(home, ".durable", instructionsFile), "~/.durable/"+instructionsFile, true)
	}
	for _, d := range dirs {
		rel, err := filepath.Rel(root, d)
		if err != nil {
			return instructions{}, fmt.Errorf("failed to relate %q to the repository root: %w", d, err)
		}
		add(filepath.Join(d, instructionsFile), filepath.ToSlash(filepath.Join(rel, instructionsFile)), false)
	}
	res.Text = strings.Join(blocks, "\n\n")
	return res, nil
}

// projectDirs returns the repository root (the nearest ancestor with .git) and the directories from it down to cwd.
func projectDirs(cwd string) (string, []string) {
	dirs := []string{cwd}
	for d := cwd; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
				dirs[i], dirs[j] = dirs[j], dirs[i]
			}
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

// readInstructions returns the file text, whether it was cut at limit, and a reason if it must be skipped.
// A missing file is normal and returns no reason.
func readInstructions(path, root string, trusted bool, limit int) (string, bool, string) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, ""
		}
		return "", false, err.Error()
	}
	if !trusted {
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", false, "it resolves outside the repository"
		}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", false, err.Error()
	}
	if !info.Mode().IsRegular() {
		return "", false, "not a regular file"
	}
	f, err := os.Open(resolved)
	if err != nil {
		return "", false, err.Error()
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return "", false, err.Error()
	}
	if len(b) > limit {
		return strings.ToValidUTF8(string(b[:limit]), ""), true, ""
	}
	return string(b), false, ""
}

// pinInstructions fixes the session's instruction text on first start; a later call returns the stored text.
// existed says session.sqlite predates this run, so an older session is pinned empty.
func pinInstructions(dir string, existed, noContext bool, discover func() (instructions, error)) (instructions, error) {
	pin := filepath.Join(dir, pinFile)
	switch b, err := os.ReadFile(pin); { //nolint:gosec // pin is a fixed name inside the session directory
	case err == nil:
		return instructions{Text: string(b)}, nil
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

	tmp, err := os.CreateTemp(dir, "instructions-*.tmp")
	if err != nil {
		return instructions{}, fmt.Errorf("failed to create pin file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return instructions{}, fmt.Errorf("failed to set pin file mode: %w", err)
	}
	if _, err := tmp.WriteString(res.Text); err != nil {
		_ = tmp.Close()
		return instructions{}, fmt.Errorf("failed to write pin file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return instructions{}, fmt.Errorf("failed to sync pin file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return instructions{}, fmt.Errorf("failed to close pin file: %w", err)
	}

	// Link fails if another process pinned first; its text wins so both agree.
	if err := os.Link(tmp.Name(), pin); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return instructions{}, fmt.Errorf("failed to pin instructions: %w", err)
		}
		b, err := os.ReadFile(pin) //nolint:gosec // pin is a fixed name inside the session directory
		if err != nil {
			return instructions{}, fmt.Errorf("failed to read pinned instructions: %w", err)
		}
		return instructions{Text: string(b)}, nil
	}
	return res, nil
}
