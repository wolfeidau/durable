package main

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/fakeapi"
)

func writeSkill(t *testing.T, skillsDir, name, desc, extra string) {
	t.Helper()
	dir := filepath.Join(skillsDir, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	text := "---\nname: " + name + "\ndescription: " + desc + "\n" + extra + "---\n\nBody of " + name + ".\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(text), 0o644))
}

func userSkills(home string) string { return filepath.Join(home, ".durable", "skills") }

func skillCall(t *testing.T, name string, path any) string {
	t.Helper()
	input, err := json.Marshal(map[string]any{"name": name, "path": path})
	require.NoError(t, err)
	return fakeapi.Stream(apiModel, "tool_use", "null", fakeapi.ToolBlock(0, "toolu_1", "skill", string(input)))
}

// chatOnce runs one prompt against a canned first reply and returns the agent and API.
func chatOnce(t *testing.T, bin, ws, home, reply, prompt string, wantRequests int, args ...string) (*agent, *fakeAPI) {
	t.Helper()
	api := newFakeAPIReply(t, reply)
	a := startAgent(t, bin, ws, home, api.srv.URL, args...)
	runPrompt(t, api, wantRequests, a, prompt)
	return a, api
}

func TestSkillAppearsInToolsWithoutChangingSystem(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	_, base := chatOnce(t, bin, ws, home, textReply("hi"), "go", 1)

	ws2, home2 := newWorkspace(t)
	writeSkill(t, userSkills(home2), "demo", "Demonstrates things.", "")
	a, api := chatOnce(t, bin, ws2, home2, textReply("hi"), "go", 1)

	first := api.requests()[0]
	tool := toolByName(t, first, "skill")
	require.NotNil(t, tool, first)
	require.Equal(t, true, tool["strict"])
	require.Contains(t, tool["description"], "- demo: Demonstrates things.")
	require.Equal(t, systemOf(t, base.requests()[0]), systemOf(t, first))
	require.Contains(t, a.out.String(), "[skills] 1 skills: demo")
}

func TestNoSkillsLeavesRequestUnchanged(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	_, plain := chatOnce(t, bin, ws, home, textReply("hi"), "go", 1)

	ws2, home2 := newWorkspace(t)
	for _, d := range []string{userSkills(home2), filepath.Join(home2, ".agents", "skills"), filepath.Join(ws2, ".agents", "skills")} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	a, empty := chatOnce(t, bin, ws2, home2, textReply("hi"), "go", 1)

	require.Nil(t, toolByName(t, empty.requests()[0], "skill"))
	require.Equal(t, toolsOf(t, plain.requests()[0]), toolsOf(t, empty.requests()[0]))
	require.NotContains(t, a.out.String(), "[skills]")
}

func TestSkillToolRunsWithoutApproval(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	writeSkill(t, userSkills(home), "demo", "Demonstrates things.", "")
	a, api := chatOnce(t, bin, ws, home, skillCall(t, "demo", nil), "load it", 2)

	require.NotContains(t, a.out.String(), "[y/N]")
	require.True(t, hasToolResult(t, api.requests()[1], false, "Body of demo."), api.requests()[1])
}

func TestSkillToolRefusesPathEscape(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	writeSkill(t, userSkills(home), "demo", "Demonstrates things.", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, ".durable", "secret"), []byte("TOPSECRETVALUE"), 0o600))
	_, api := chatOnce(t, bin, ws, home, skillCall(t, "demo", "../../secret"), "load it", 2)

	reqs := api.requests()
	require.True(t, hasToolResult(t, reqs[1], true, ""), reqs[1])
	for _, r := range reqs {
		require.NotContains(t, r, "TOPSECRETVALUE")
	}
}

func TestSkillCatalogIsPinnedAcrossRuns(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	writeSkill(t, userSkills(home), "demo", "Demonstrates things.", "")
	api := newFakeAPIReply(t, textReply("hi"))

	a := startAgent(t, bin, ws, home, api.srv.URL)
	runPrompt(t, api, 1, a, "first")

	writeSkill(t, userSkills(home), "later", "Added afterwards.", "")
	b := startAgent(t, bin, ws, home, api.srv.URL)
	runPrompt(t, api, 2, b, "second")
	require.Contains(t, b.out.String(), "[skills] using 1 pinned skills; start a new --session to reload skills")

	reqs := api.requests()
	require.Equal(t, toolsOf(t, reqs[0]), toolsOf(t, reqs[1]))
	require.NotContains(t, toolsOf(t, reqs[1]), "later")
}

func TestNoContextSkipsSkills(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	writeSkill(t, userSkills(home), "demo", "Demonstrates things.", "")
	a, api := chatOnce(t, bin, ws, home, textReply("hi"), "go", 1, "--no-context")

	require.Nil(t, toolByName(t, api.requests()[0], "skill"))
	require.NotContains(t, a.out.String(), "[skills]")
}

func TestSkillSlashCommandExpandsHiddenSkill(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	writeSkill(t, userSkills(home), "secretive", "User only.", "disable-model-invocation: true\n")
	a, api := chatOnce(t, bin, ws, home, textReply("hi"), "/skill:secretive do it", 1)

	first := api.requests()[0]
	require.Nil(t, toolByName(t, first, "skill"))
	require.Contains(t, a.out.String(), "[skills] 1 skills: secretive")
	hist := finalHistory(t, ws)
	require.Equal(t, durable.RoleUser, hist[0].Role)
	require.Contains(t, hist[0].Text, "Body of secretive.")
	require.Contains(t, hist[0].Text, "do it")
}

func TestUnknownSkillSlashCommandSubmitsNothing(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	api := newFakeAPIReply(t, textReply("hi"))
	a := startAgent(t, bin, ws, home, api.srv.URL)
	_, err := io.WriteString(a.stdin, "/skill:nope\nplain\n")
	require.NoError(t, err)
	eventually(t, "API requests", func() bool { return len(api.requests()) == 1 })
	require.NoError(t, a.stdin.Close())
	var exitErr *exec.ExitError
	require.ErrorAs(t, a.cmd.Wait(), &exitErr, a.out.String())
	require.Contains(t, a.out.String(), `unknown skill "nope"`)
	require.Contains(t, api.requests()[0], "plain")
	require.NotContains(t, api.requests()[0], "/skill:")
}

func TestProjectSkillShadowsUserAndEscapeIsSkipped(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	require.NoError(t, os.Mkdir(filepath.Join(ws, ".git"), 0o755))
	writeSkill(t, userSkills(home), "x", "user version", "")
	writeSkill(t, filepath.Join(ws, ".agents", "skills"), "x", "project version", "")
	outside := filepath.Join(home, "outside")
	writeSkill(t, outside, "esc", "escaped skill", "")
	require.NoError(t, os.Symlink(filepath.Join(outside, "esc"), filepath.Join(ws, ".agents", "skills", "esc")))

	a, api := chatOnce(t, bin, ws, home, textReply("hi"), "go", 1)

	desc, _ := toolByName(t, api.requests()[0], "skill")["description"].(string)
	require.Contains(t, desc, "- x: project version")
	require.NotContains(t, desc, "user version")
	require.NotContains(t, desc, "escaped skill")
	require.Contains(t, a.out.String(), "[skills] warning: skills: ")
	require.Contains(t, a.out.String(), "shadowed")
	require.Contains(t, a.out.String(), "skills/esc: ")
}

func TestSessionThatPredatesSkillsGetsNone(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	api := newFakeAPIReply(t, textReply("hi"))
	a := startAgent(t, bin, ws, home, api.srv.URL)
	runPrompt(t, api, 1, a, "first")

	pins, err := filepath.Glob(filepath.Join(home, ".durable", "sessions", "*", "skills.json"))
	require.NoError(t, err)
	require.Len(t, pins, 1)
	require.NoError(t, os.Remove(pins[0]))
	writeSkill(t, userSkills(home), "demo", "Demonstrates things.", "")

	b := startAgent(t, bin, ws, home, api.srv.URL)
	runPrompt(t, api, 2, b, "second")
	require.Nil(t, toolByName(t, api.requests()[1], "skill"))
	require.NotContains(t, b.out.String(), "[skills]")
}
