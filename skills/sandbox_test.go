package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

func mapFiles(m fstest.MapFS) FilesFunc { return func(context.Context) fs.FS { return m } }

func sandboxFS() fstest.MapFS {
	return fstest.MapFS{
		".agents/skills/pdf/SKILL.md":       {Data: []byte(skillMD("pdf", "Do PDFs."))},
		".agents/skills/pdf/scripts/run.sh": {Data: []byte("echo hi\n")},
		".agents/skills/pdf/.git/config":    {Data: []byte("x")},
		".agents/skills/pdf/.git/HEAD":      {Data: []byte("x")},
		".agents/skills/notes.txt":          {Data: []byte("stray")},
		".agents/skills/empty/readme":       {Data: []byte("not a skill")},
	}
}

func sandboxSource(m fstest.MapFS) Source {
	return Source{Dir: ".agents/skills", Root: "/workspace", Files: mapFiles(m)}
}

func TestDiscoverReadsASandboxSource(t *testing.T) {
	m := sandboxFS()
	got, warns := Discover(t.Context(), []Source{sandboxSource(m), {Dir: ".durable/skills", Root: "/workspace", Files: mapFiles(m)}})

	require.Empty(t, warns)
	require.Equal(t, []Skill{{Name: "pdf", Description: "Do PDFs.", Root: "/workspace", Dir: ".agents/skills/pdf", Sandbox: true}}, got)
}

func TestDiscoverSandboxSourceMissingIsSilent(t *testing.T) {
	got, warns := Discover(t.Context(), []Source{{Dir: ".durable/skills", Root: "/workspace", Files: mapFiles(fstest.MapFS{})}})

	require.Empty(t, got)
	require.Empty(t, warns)
}

func TestSandboxSkillRendersThroughTheFileSystem(t *testing.T) {
	m := sandboxFS()
	ss, _ := Discover(t.Context(), []Source{sandboxSource(m)})
	tool := Tool(ss, mapFiles(m))

	out, err := run(t, tool, `{"name":"pdf","path":null}`)

	require.NoError(t, err)
	require.Contains(t, out, "Skill directory: /workspace/.agents/skills/pdf")
	require.Contains(t, out, "<file>scripts/run.sh</file>")
	require.NotContains(t, out, ".git")

	out, err = run(t, tool, `{"name":"pdf","path":"scripts/run.sh"}`)
	require.NoError(t, err)
	require.Equal(t, "echo hi\n", out)

	_, err = run(t, tool, `{"name":"pdf","path":"../notes.txt"}`)
	require.ErrorContains(t, err, "must be relative")
}

func TestSandboxSkillCapsItsResourceListing(t *testing.T) {
	m := fstest.MapFS{".agents/skills/big/SKILL.md": {Data: []byte(skillMD("big", "Many files."))}}
	for i := range maxResources + 5 {
		m[fmt.Sprintf(".agents/skills/big/res/f%03d", i)] = &fstest.MapFile{Data: []byte("x")}
	}
	ss, _ := Discover(t.Context(), []Source{sandboxSource(m)})

	out, err := run(t, Tool(ss, mapFiles(m)), `{"name":"big","path":null}`)

	require.NoError(t, err)
	require.Contains(t, out, "[more than 100 files")
	require.Equal(t, maxResources, strings.Count(out, "<file>"))
}

func TestSandboxSkillWithoutFilesFails(t *testing.T) {
	m := sandboxFS()
	ss, _ := Discover(t.Context(), []Source{sandboxSource(m)})
	tool := Tool(ss, nil)

	_, err := run(t, tool, `{"name":"pdf","path":null}`)
	require.ErrorContains(t, err, "sandbox")
	_, err = run(t, tool, `{"name":"pdf","path":"scripts/run.sh"}`)
	require.ErrorContains(t, err, "sandbox")
	_, err = Expand(t.Context(), ss[0], "x", nil)
	require.ErrorContains(t, err, "sandbox")
}

func TestExpandReadsASandboxSkill(t *testing.T) {
	m := sandboxFS()
	ss, _ := Discover(t.Context(), []Source{sandboxSource(m)})

	out, err := Expand(t.Context(), ss[0], "do it", mapFiles(m))

	require.NoError(t, err)
	require.Contains(t, out, "<skill_content")
	require.Contains(t, out, "do it")
}

func TestSkillPinWithoutSandboxFieldDecodes(t *testing.T) {
	var s Skill
	require.NoError(t, json.Unmarshal([]byte(`{"name":"a","description":"d","root":"/r","dir":"."}`), &s))
	require.False(t, s.Sandbox)

	b, err := json.Marshal(Skill{Name: "a", Description: "d", Root: "/r", Dir: "."})
	require.NoError(t, err)
	require.NotContains(t, string(b), "sandbox")
}
