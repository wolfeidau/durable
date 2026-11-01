package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/tools"
)

const (
	maxResultBytes = 30 * 1024
	maxResources   = 100
)

var _ durable.ReadOnlyTool = (*skillTool)(nil)

// Find returns the skill with the given name, hidden or not.
func Find(skills []Skill, name string) (Skill, bool) {
	i := slices.IndexFunc(skills, func(s Skill) bool { return s.Name == name })
	if i < 0 {
		return Skill{}, false
	}
	return skills[i], true
}

// Tool returns the skill tool for the non-hidden skills, or nil when there are none.
func Tool(skills []Skill) durable.Tool {
	var visible []Skill
	for _, s := range skills {
		if !s.Hidden {
			visible = append(visible, s)
		}
	}
	if len(visible) == 0 {
		return nil
	}
	sort.SliceStable(visible, func(i, j int) bool { return visible[i].Name < visible[j].Name })

	names := make([]string, len(visible))
	var desc strings.Builder
	desc.WriteString("Load a skill: specialised instructions for a task. When a task matches a skill's description, load it with this tool before proceeding. " +
		"Relative paths in a skill are resolved by passing them as `path` (or null to load the skill itself); run its scripts through bash using the skill directory shown.\n\nAvailable skills:\n")
	for i, s := range visible {
		names[i] = s.Name
		fmt.Fprintf(&desc, "- %s: %s\n", s.Name, oneLine(s.Description))
	}
	enum, _ := json.Marshal(names)
	return &skillTool{
		spec: durable.ToolSpec{
			Name:        "skill",
			Description: strings.TrimRight(desc.String(), "\n"),
			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"name":{"type":"string","enum":` + string(enum) + `,"description":"The skill to load."},` +
				`"path":{"type":["string","null"],"description":"A file inside the skill directory, relative to it, or null to load the skill."}},` +
				`"required":["name","path"],"additionalProperties":false}`),
		},
		skills: visible,
	}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

type skillTool struct {
	spec   durable.ToolSpec
	skills []Skill
}

func (t *skillTool) Spec() durable.ToolSpec { return t.spec }
func (t *skillTool) Replay() durable.Replay { return durable.ReplaySafe }
func (t *skillTool) ReadOnly() bool         { return true }

func (t *skillTool) Execute(ctx context.Context, call durable.ToolCall) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	trimmed := bytes.TrimSpace(call.Input)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", errors.New("invalid tool input: name and path are required")
	}
	var in struct {
		Name string  `json:"name"`
		Path *string `json:"path"`
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return "", fmt.Errorf("invalid tool input: %w", err)
	}
	s, ok := Find(t.skills, in.Name)
	if !ok {
		return "", fmt.Errorf("unknown skill %q", in.Name)
	}
	if in.Path == nil {
		return render(ctx, s)
	}
	if !filepath.IsLocal(*in.Path) {
		return "", fmt.Errorf("path %q must be relative and stay inside the skill directory", *in.Path)
	}
	text, truncated, err := readText(s.Root, filepath.Join(s.Dir, *in.Path))
	if err != nil {
		return "", err
	}
	return finish(text, truncated), nil
}

// Expand wraps a skill for a user-invoked turn, followed by the user's text. It works for hidden skills.
func Expand(s Skill, text string) (string, error) {
	out, err := render(context.Background(), s)
	if err != nil {
		return "", err
	}
	if text != "" {
		out += "\n\n" + text
	}
	return out, nil
}

func render(ctx context.Context, s Skill) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, _, err := readConfined(s.Root, filepath.Join(s.Dir, "SKILL.md"))
	if err != nil {
		return "", err
	}
	_, body, ok := split(string(data))
	if !ok {
		return "", fmt.Errorf("skill %q has no frontmatter", s.Name)
	}
	files, capped, err := listResources(ctx, s)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "<skill_content name=%q>\n%s\n\nSkill directory: %s\nRelative paths in this skill are relative to the skill directory.\n<skill_resources>\n",
		s.Name, truncate(body), filepath.Join(s.Root, s.Dir))
	for _, f := range files {
		fmt.Fprintf(&sb, "<file>%s</file>\n", f)
	}
	if capped {
		fmt.Fprintf(&sb, "[more than %d files; list the directory with bash to see the rest]\n", maxResources)
	}
	sb.WriteString("</skill_resources>\n</skill_content>")
	return sb.String(), nil
}

// listResources lists files under the skill directory without reading them or leaving Root.
func listResources(ctx context.Context, s Skill) ([]string, bool, error) {
	r, err := os.OpenRoot(s.Root)
	if err != nil {
		return nil, false, fmt.Errorf("failed to open root: %w", err)
	}
	defer r.Close()
	sub, err := r.OpenRoot(s.Dir)
	if err != nil {
		return nil, false, fmt.Errorf("failed to open skill directory: %w", err)
	}
	defer sub.Close()

	var files []string
	capped := false
	err = fs.WalkDir(sub.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if p == "SKILL.md" {
			return nil
		}
		if len(files) == maxResources {
			capped = true
			return fs.SkipAll
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("failed to list skill directory: %w", err)
	}
	return files, capped, nil
}

func readText(root, rel string) (string, bool, error) {
	b, truncated, err := readConfined(root, rel)
	if err != nil {
		return "", false, err
	}
	return truncate(string(b)), truncated, nil
}

// truncate cuts to the result cap; the cut can split a rune.
func truncate(s string) string {
	return strings.ToValidUTF8(tools.Truncate(s, maxResultBytes), "")
}

func finish(text string, truncated bool) string {
	if truncated {
		return text + "\n[file truncated: only the first 1 MiB was read]"
	}
	return text
}
