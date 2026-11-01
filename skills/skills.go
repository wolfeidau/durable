// Package skills discovers Agent Skills (https://agentskills.io/specification) and exposes them to the model.
package skills

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

const (
	maxSkills      = 64
	maxNameLen     = 64
	maxDescription = 1024
	maxFileBytes   = 1 << 20
)

var validName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Skill is one discovered skill. It is persisted per session, so it must round-trip as JSON.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Root is the absolute boundary every read of the skill stays inside.
	Root string `json:"root"`
	// Dir is the skill directory relative to Root; "." is the root itself.
	Dir string `json:"dir"`
	// Hidden skills are for users to invoke only (disable-model-invocation).
	Hidden bool `json:"hidden,omitempty"`
}

// Source is a directory of skills to scan.
type Source struct {
	// Dir is the absolute skills directory.
	Dir string
	// Root is the confinement boundary for a project source. Empty means a user
	// source, where each skill's resolved directory is its own root.
	Root string
}

type candidate struct {
	skill Skill
	from  string
}

// Discover scans the sources in precedence order, where the first skill of a name wins.
// The warnings are safe to print.
func Discover(sources []Source) ([]Skill, []string) {
	var (
		warns []string
		found []candidate
		seen  = map[string]string{}
	)
	for _, src := range sources {
		entries, err := os.ReadDir(src.Dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			warns = append(warns, fmt.Sprintf("skills: cannot read %s: %v", src.Dir, err))
			continue
		}
		for _, e := range entries {
			s, ws, ok := load(src, e.Name())
			warns = append(warns, ws...)
			if !ok {
				continue
			}
			from := filepath.Join(src.Dir, e.Name())
			if prev, dup := seen[s.Name]; dup {
				warns = append(warns, fmt.Sprintf("skills: %s is shadowed by %s (both named %q)", from, prev, s.Name))
				continue
			}
			seen[s.Name] = from
			found = append(found, candidate{s, from})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].skill.Name < found[j].skill.Name })

	out := make([]Skill, 0, len(found))
	visible := 0
	for _, c := range found {
		if !c.skill.Hidden {
			if visible == maxSkills {
				warns = append(warns, fmt.Sprintf("skills: dropped %s (%q): more than %d skills", c.from, c.skill.Name, maxSkills))
				continue
			}
			visible++
		}
		out = append(out, c.skill)
	}
	return out, warns
}

// load reads one candidate directory. A directory without SKILL.md is not a skill and is silent.
func load(src Source, dirName string) (Skill, []string, bool) {
	path := filepath.Join(src.Dir, dirName)
	root, rel := src.Root, ""
	if root == "" {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return Skill{}, nil, false
		}
		if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
			return Skill{}, nil, false
		}
		root, rel = resolved, "."
	} else {
		r, err := filepath.Rel(root, path)
		if err != nil || !filepath.IsLocal(r) {
			return Skill{}, []string{fmt.Sprintf("skills: skipped %s: outside %s", path, root)}, false
		}
		rel = r
	}

	data, _, err := readConfined(root, filepath.Join(rel, "SKILL.md"))
	if errors.Is(err, fs.ErrNotExist) {
		return Skill{}, nil, false
	}
	if err != nil {
		return Skill{}, []string{fmt.Sprintf("skills: skipped %s: %v", path, err)}, false
	}

	s, ws, ok := parse(string(data), dirName, path)
	s.Root, s.Dir = root, rel
	return s, ws, ok
}

type meta struct {
	Name        any `yaml:"name"`
	Description any `yaml:"description"`
	Hidden      any `yaml:"disable-model-invocation"`
}

// parse applies the lenient rules: only a missing description or frontmatter rejects a skill.
func parse(text, dirName, path string) (Skill, []string, bool) {
	fm, _, ok := split(text)
	if !ok {
		return Skill{}, []string{fmt.Sprintf("skills: skipped %s: no frontmatter", path)}, false
	}
	var warns []string
	var m meta
	if err := yaml.Unmarshal([]byte(fm), &m); err != nil {
		warns = append(warns, fmt.Sprintf("skills: %s: invalid YAML, reading name and description line by line", path))
		m = scan(fm)
	}

	name, _ := m.Name.(string)
	name = strings.TrimSpace(name)
	if name == "" {
		name = dirName
		warns = append(warns, fmt.Sprintf("skills: %s: no name, using directory name", path))
	}
	desc, _ := m.Description.(string)
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return Skill{}, append(warns, fmt.Sprintf("skills: skipped %s: no description", path)), false
	}

	switch {
	case len(name) > maxNameLen || !validName.MatchString(name):
		warns = append(warns, fmt.Sprintf("skills: %s: name %q is not valid (lowercase letters, digits and hyphens, at most %d characters)", path, name, maxNameLen))
	case name != dirName:
		warns = append(warns, fmt.Sprintf("skills: %s: name %q does not match its directory", path, name))
	}
	if len(desc) > maxDescription {
		cut := maxDescription
		for cut > 0 && !utf8.RuneStart(desc[cut]) {
			cut--
		}
		desc = desc[:cut]
		warns = append(warns, fmt.Sprintf("skills: %s: description cut to %d bytes", path, maxDescription))
	}
	hidden, _ := m.Hidden.(bool)
	return Skill{Name: name, Description: desc, Hidden: hidden}, warns, true
}

// scan reads top-level key: value lines; unquoted colons are the usual reason YAML fails.
func scan(fm string) meta {
	var m meta
	for line := range strings.SplitSeq(fm, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		switch strings.TrimSpace(k) {
		case "name":
			m.Name = v
		case "description":
			m.Description = v
		}
	}
	return m
}

// split returns the frontmatter and trimmed body of a SKILL.md.
func split(text string) (string, string, bool) {
	text = strings.ReplaceAll(strings.TrimPrefix(text, "\ufeff"), "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return "", "", false
	}
	var fm strings.Builder
	for {
		line, after, more := strings.Cut(rest, "\n")
		if strings.TrimRight(line, " \t") == "---" {
			return fm.String(), strings.TrimSpace(after), true
		}
		if !more {
			return "", "", false
		}
		fm.WriteString(line)
		fm.WriteByte('\n')
		rest = after
	}
}

// readConfined reads rel inside root through one handle, so a symlink or swap can't leave root.
// It reports whether the file was cut at maxFileBytes.
func readConfined(root, rel string) ([]byte, bool, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, false, fmt.Errorf("failed to open root: %w", err)
	}
	defer r.Close()
	// O_NONBLOCK stops a FIFO from hanging the open.
	f, err := r.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
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
