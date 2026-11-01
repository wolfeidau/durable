package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

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
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve working directory: %w", err)
	}
	return skillSourcesFor(resolved, home, uint32(os.Geteuid())), nil //nolint:gosec // uids fit in 32 bits
}

func skillSourcesFor(cwd, home string, uid uint32) []skills.Source {
	root, dirs := projectDirs(cwd, uid)
	var srcs []skills.Source
	for _, d := range slices.Backward(dirs) {
		for _, sub := range []string{".durable", ".agents"} {
			srcs = append(srcs, skills.Source{Dir: filepath.Join(d, sub, "skills"), Root: root})
		}
	}
	for _, sub := range []string{".durable", ".agents"} {
		srcs = append(srcs, skills.Source{Dir: filepath.Join(home, sub, "skills")})
	}
	return srcs
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
