package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/githost/githosttest"
	"github.com/wolfeidau/durable/internal/sandbox/sandboxtest"
	"github.com/wolfeidau/durable/skills"
)

const demoSkill = "---\nname: demo\ndescription: A demo skill.\n---\nDemo body."

func projectContext() []githosttest.Option {
	return []githosttest.Option{
		githosttest.WithFile("AGENTS.md", "Repo rules.\n"),
		githosttest.WithFile(".agents/skills/demo/SKILL.md", demoSkill),
		githosttest.WithFile(".agents/skills/demo/scripts/run.sh", "echo hi\n"),
	}
}

func skillTool(t *testing.T, a *Assembled) durable.Tool {
	t.Helper()
	for _, tl := range a.toolset {
		if tl.Spec().Name == "skill" {
			return tl
		}
	}
	t.Fatal("no skill tool")
	return nil
}

func runSkill(t *testing.T, a *Assembled, input string) (string, error) {
	t.Helper()
	return skillTool(t, a).Execute(t.Context(), durable.ToolCall{ID: "1", Name: "skill", Input: json.RawMessage(input)})
}

func pinnedInstructions(t *testing.T, a *Assembled) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(a.Path), pinFile))
	require.NoError(t, err)
	return string(b)
}

func TestAssembleRepoSessionPinsProjectInstructionsAndSkills(t *testing.T) {
	assembleHome(t)
	f := sandboxtest.New(t)
	h := githosttest.New(t, projectContext()...)
	cfg := repoCfg(t, f, h, "main", true)

	a, err := Assemble(t.Context(), cfg)
	require.NoError(t, err)

	root := f.Root(repoPin(t, a).Name)
	require.Contains(t, pinnedInstructions(t, a), "<instructions path=\"AGENTS.md\">\nRepo rules.\n</instructions>")
	require.Empty(t, a.instructions.Warnings)
	require.Equal(t, []skills.Skill{{Name: "demo", Description: "A demo skill.", Root: root, Dir: ".agents/skills/demo", Sandbox: true}}, a.skills.Skills)
	pinned, err := os.ReadFile(filepath.Join(filepath.Dir(a.Path), skillsPinFile))
	require.NoError(t, err)
	require.Contains(t, string(pinned), `"sandbox":true`)

	out, err := runSkill(t, a, `{"name":"demo","path":null}`)
	require.NoError(t, err)
	require.Contains(t, out, "Demo body.")
	require.Contains(t, out, "Skill directory: "+filepath.Join(root, ".agents/skills/demo"))
	require.Contains(t, out, "<file>scripts/run.sh</file>")
	out, err = runSkill(t, a, `{"name":"demo","path":"scripts/run.sh"}`)
	require.NoError(t, err)
	require.Equal(t, "echo hi\n", out)

	// The skill is read when called, so an edit in the sandbox shows without changing the pin.
	require.NoError(t, os.WriteFile(filepath.Join(root, ".agents/skills/demo/SKILL.md"), []byte(strings.Replace(demoSkill, "Demo body.", "Edited body.", 1)), 0o600))
	out, err = runSkill(t, a, `{"name":"demo","path":null}`)
	require.NoError(t, err)
	require.Contains(t, out, "Edited body.")
	a.Close()

	require.NoError(t, os.WriteFile(a.Path, nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("Changed rules.\n"), 0o600))
	b, err := Assemble(t.Context(), cfg)
	require.NoError(t, err)
	defer b.Close()
	require.True(t, b.instructions.Pinned)
	require.Contains(t, b.instructions.Text, "Repo rules.")
	require.NotContains(t, b.instructions.Text, "Changed rules.")
	require.Equal(t, a.skills.Skills, b.skills.Skills)
	out, err = runSkill(t, b, `{"name":"demo","path":null}`)
	require.NoError(t, err)
	require.Contains(t, out, "Edited body.")
}

func TestAssembleRepoSessionProjectSkillsBeatUserSkills(t *testing.T) {
	assembleHome(t)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	writeFile(t, filepath.Join(home, ".durable", "AGENTS.md"), "user rules")
	writeFile(t, filepath.Join(home, ".durable", "skills", "demo", "SKILL.md"), "---\nname: demo\ndescription: user one\n---\nUser body.")
	writeFile(t, filepath.Join(home, ".durable", "skills", "mine", "SKILL.md"), "---\nname: mine\ndescription: user only\n---\nMine.")
	f := sandboxtest.New(t)
	h := githosttest.New(t, projectContext()...)

	a, err := Assemble(t.Context(), repoCfg(t, f, h, "main", true))
	require.NoError(t, err)
	defer a.Close()

	text := pinnedInstructions(t, a)
	require.Contains(t, text, "user rules")
	require.Contains(t, text, "Repo rules.")
	require.Less(t, strings.Index(text, "user rules"), strings.Index(text, "Repo rules."))
	require.Len(t, a.skills.Skills, 2)
	byName := map[string]skills.Skill{}
	for _, s := range a.skills.Skills {
		byName[s.Name] = s
	}
	require.True(t, byName["demo"].Sandbox)
	require.False(t, byName["mine"].Sandbox)
	out, err := runSkill(t, a, `{"name":"mine","path":null}`)
	require.NoError(t, err)
	require.Contains(t, out, "Mine.")
}

func TestAssembleRepoSessionWarnsAboutAnOversizedAGENTSFile(t *testing.T) {
	assembleHome(t)
	h := githosttest.New(t, githosttest.WithFile("AGENTS.md", strings.Repeat("rule\n", 10<<10)))

	a, err := Assemble(t.Context(), repoCfg(t, sandboxtest.New(t), h, "main", true))
	require.NoError(t, err)
	defer a.Close()

	require.Len(t, a.instructions.Warnings, 1)
	require.Contains(t, a.instructions.Warnings[0], "AGENTS.md truncated")
	require.Contains(t, pinnedInstructions(t, a), truncationMarker)
}

func TestAssembleRepoSessionRefusesABinaryAGENTSFile(t *testing.T) {
	assembleHome(t)
	h := githosttest.New(t, githosttest.WithFile("AGENTS.md", "bin\x00ary"))

	a, err := Assemble(t.Context(), repoCfg(t, sandboxtest.New(t), h, "main", true))
	require.NoError(t, err)
	defer a.Close()

	require.Equal(t, []string{"AGENTS.md skipped: binary file"}, a.instructions.Warnings)
	require.Empty(t, a.instructions.Text)
}

func TestAssembleRepoSessionRefusesAnAGENTSSymlinkOutOfTheWorkspace(t *testing.T) {
	assembleHome(t)
	secret := filepath.Join(t.TempDir(), "secret.md")
	writeFile(t, secret, "do not read")
	h := githosttest.New(t, githosttest.WithSymlink("AGENTS.md", secret))

	a, err := Assemble(t.Context(), repoCfg(t, sandboxtest.New(t), h, "main", true))
	require.NoError(t, err)
	defer a.Close()

	require.Len(t, a.instructions.Warnings, 1)
	require.Contains(t, a.instructions.Warnings[0], "AGENTS.md skipped")
	require.NotContains(t, pinnedInstructions(t, a), "do not read")
	require.Empty(t, a.instructions.Text)
}

func TestAssembleRepoSessionWithoutProjectFilesPinsNothing(t *testing.T) {
	assembleHome(t)

	a, err := Assemble(t.Context(), repoCfg(t, sandboxtest.New(t), githosttest.New(t), "main", true))
	require.NoError(t, err)
	defer a.Close()

	require.Empty(t, a.instructions.Text)
	require.Empty(t, a.instructions.Warnings)
	require.Empty(t, a.skills.Skills)
}

func TestAssembleRepoSessionNoContextReadsNothing(t *testing.T) {
	assembleHome(t)
	f := sandboxtest.New(t)
	cfg := repoCfg(t, f, githosttest.New(t, projectContext()...), "main", true)
	cfg.NoContext = true

	a, err := Assemble(t.Context(), cfg)
	require.NoError(t, err)
	defer a.Close()

	require.Empty(t, a.instructions.Text)
	require.Empty(t, a.skills.Skills)
}
