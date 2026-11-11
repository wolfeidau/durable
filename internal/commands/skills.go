package commands

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/skills"
)

const skillsPinFile = "skills.json"

// skillSet is the session's skill catalog plus what to tell the user about how it was built.
type skillSet struct {
	Skills   []skills.Skill
	Warnings []string
	// Pinned means the catalog came from an earlier pin rather than fresh discovery.
	Pinned bool
}

// skillSources lists skill directories in precedence order: project directories nearest first, then the user's.
func skillSources(cwd, home string) ([]skills.Source, error) {
	// An empty cwd (an empty session) has no project; EvalSymlinks("") would resolve to the process cwd.
	if cwd == "" {
		return userSkillSources(home), nil
	}
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve working directory: %w", err)
	}
	return skillSourcesFor(resolved, home, uint32(os.Geteuid())), nil //nolint:gosec // uids fit in 32 bits
}

// workspaceSkillSources is skillSources for a repo session: the clone's skill directories, then the user's.
// root is the workspace's path in the sandbox, which only the skill directory shown to the model uses.
func workspaceSkillSources(root string, files skills.FilesFunc, home string) []skills.Source {
	var srcs []skills.Source
	for _, sub := range []string{".durable", ".agents"} {
		srcs = append(srcs, skills.Source{Dir: sub + "/skills", Root: root, Files: files})
	}
	return append(srcs, userSkillSources(home)...)
}

func userSkillSources(home string) []skills.Source {
	var srcs []skills.Source
	for _, sub := range []string{".durable", ".agents"} {
		srcs = append(srcs, skills.Source{Dir: filepath.Join(home, sub, "skills")})
	}
	return srcs
}

func skillSourcesFor(cwd, home string, uid uint32) []skills.Source {
	root, dirs := projectDirs(cwd, uid)
	var srcs []skills.Source
	for _, d := range slices.Backward(dirs) {
		for _, sub := range []string{".durable", ".agents"} {
			srcs = append(srcs, skills.Source{Dir: filepath.Join(d, sub, "skills"), Root: root})
		}
	}
	// Only the user form can load a symlinked skill, so it wins when cwd is home.
	userDirs := map[string]bool{}
	for _, sub := range []string{".durable", ".agents"} {
		dir := filepath.Join(home, sub, "skills")
		userDirs[canonical(filepath.Join(canonical(home), sub, "skills"))] = true
		srcs = append(srcs, skills.Source{Dir: dir})
	}
	return slices.DeleteFunc(srcs, func(s skills.Source) bool {
		return s.Root != "" && userDirs[canonical(s.Dir)]
	})
}

func canonical(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// pinSkills pins the catalog on first start; existed (session.sqlite predates this run) pins an older session empty.
func pinSkills(dir string, existed, noContext bool, discover func() ([]skills.Skill, []string, error)) (skillSet, error) {
	var warnings []string
	found, pinned, err := pinJSON(dir, skillsPinFile, "skills", existed, []skills.Skill{}, func() ([]skills.Skill, error) {
		if noContext {
			return []skills.Skill{}, nil
		}
		var list []skills.Skill
		var err error
		list, warnings, err = discover()
		if list == nil {
			list = []skills.Skill{}
		}
		return list, err
	})
	if err != nil {
		return skillSet{}, err
	}
	if pinned {
		warnings = nil
	}
	return skillSet{Skills: found, Warnings: warnings, Pinned: pinned}, nil
}

// workerWatch passes sandbox reads through and keeps the first failure of the worker itself.
type workerWatch struct{ err error }

func (w *workerWatch) files(next skills.FilesFunc) skills.FilesFunc {
	return func(ctx context.Context) fs.FS { return watchedFS{fsys: next(ctx), w: w} }
}

func (w *workerWatch) note(err error) error {
	if w.err == nil && errors.Is(err, sandbox.ErrWorker) {
		w.err = err
	}
	return err
}

type watchedFS struct {
	fsys fs.FS
	w    *workerWatch
}

var (
	_ fs.ReadDirFS = watchedFS{}
	_ fs.StatFS    = watchedFS{}
)

func (f watchedFS) Open(name string) (fs.File, error) {
	file, err := f.fsys.Open(name)
	return file, f.w.note(err)
}

func (f watchedFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(f.fsys, name)
	return entries, f.w.note(err)
}

func (f watchedFS) Stat(name string) (fs.FileInfo, error) {
	info, err := fs.Stat(f.fsys, name)
	return info, f.w.note(err)
}
