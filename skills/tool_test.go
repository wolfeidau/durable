package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
)

func run(t *testing.T, tool durable.Tool, input string) (string, error) {
	t.Helper()
	return tool.Execute(context.Background(), durable.ToolCall{ID: "1", Name: "skill", Input: json.RawMessage(input)})
}

// project builds a project source with a visible skill "pdf" and a hidden skill "secret".
func project(t *testing.T) (string, []Skill) {
	t.Helper()
	root := realDir(t)
	dir := filepath.Join(root, "skills")
	write(t, filepath.Join(dir, "pdf", "SKILL.md"), skillMD("pdf", "Do PDFs."))
	write(t, filepath.Join(dir, "pdf", "scripts", "run.sh"), "echo hi\n")
	write(t, filepath.Join(dir, "pdf", ".git", "config"), "x")
	write(t, filepath.Join(dir, "secret", "SKILL.md"), "---\nname: secret\ndescription: hidden one\ndisable-model-invocation: true\n---\nSecret body.")
	got, warns := Discover([]Source{{Dir: dir, Root: root}})
	require.Empty(t, warns)
	require.Len(t, got, 2)
	return root, got
}

func TestToolSpec(t *testing.T) {
	_, ss := project(t)
	tool := Tool(ss)
	require.NotNil(t, tool)
	spec := tool.Spec()
	require.Equal(t, "skill", spec.Name)
	require.False(t, spec.NonStrict)
	require.Contains(t, spec.Description, "- pdf: Do PDFs.")
	require.NotContains(t, spec.Description, "secret")
	require.Equal(t, durable.ReplaySafe, tool.Replay())
	require.True(t, tool.(durable.ReadOnlyTool).ReadOnly())

	var schema struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type any      `json:"type"`
			Enum []string `json:"enum"`
		} `json:"properties"`
		Required             []string `json:"required"`
		AdditionalProperties bool     `json:"additionalProperties"`
	}
	require.NoError(t, json.Unmarshal(spec.Schema, &schema))
	require.Equal(t, []string{"pdf"}, schema.Properties["name"].Enum)
	require.Equal(t, []any{"string", "null"}, schema.Properties["path"].Type)
	require.ElementsMatch(t, []string{"name", "path"}, schema.Required)
	require.False(t, schema.AdditionalProperties)
}

func TestToolNilWithoutVisibleSkills(t *testing.T) {
	require.Nil(t, Tool(nil))
	require.Nil(t, Tool([]Skill{{Name: "h", Description: "d", Hidden: true}}))
}

func TestCatalogStable(t *testing.T) {
	var ss []Skill
	for _, n := range []string{"zeta", "alpha", "mid"} {
		ss = append(ss, Skill{Name: n, Description: "d " + n, Root: "/r", Dir: n})
	}
	first := Tool(ss).Spec()
	for range 5 {
		got := Tool(ss).Spec()
		require.Equal(t, first.Description, got.Description)
		require.Equal(t, string(first.Schema), string(got.Schema))
	}
	require.Less(t, strings.Index(first.Description, "alpha"), strings.Index(first.Description, "zeta"))
}

func TestExecuteLoad(t *testing.T) {
	root, ss := project(t)
	out, err := run(t, Tool(ss), `{"name":"pdf","path":null}`)
	require.NoError(t, err)
	require.Contains(t, out, `<skill_content name="pdf">`)
	require.Contains(t, out, "Body of pdf.")
	require.Contains(t, out, "Skill directory: "+filepath.Join(root, "skills", "pdf"))
	require.Contains(t, out, "<file>scripts/run.sh</file>")
	require.NotContains(t, out, "SKILL.md</file>")
	require.NotContains(t, out, ".git")
	require.True(t, strings.HasSuffix(out, "</skill_resources>\n</skill_content>"))
}

func TestExecuteReadPath(t *testing.T) {
	_, ss := project(t)
	out, err := run(t, Tool(ss), `{"name":"pdf","path":"scripts/run.sh"}`)
	require.NoError(t, err)
	require.Equal(t, "echo hi\n", out)
}

func TestExecuteBadInput(t *testing.T) {
	_, ss := project(t)
	tool := Tool(ss)
	for _, in := range []string{"", "null", "  ", "{}", `[]`, `{"name":"pdf","path":null,"x":1}`, `{"name":"nope","path":null}`, `{"name":"pdf","path":5}`, `{"name":"pdf","path":""}`} {
		_, err := tool.Execute(context.Background(), durable.ToolCall{Input: json.RawMessage(in)})
		require.Error(t, err, in)
	}
	_, err := tool.Execute(context.Background(), durable.ToolCall{})
	require.Error(t, err)
}

func TestExecuteCancelled(t *testing.T) {
	_, ss := project(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Tool(ss).Execute(ctx, durable.ToolCall{Input: json.RawMessage(`{"name":"pdf","path":null}`)})
	require.ErrorIs(t, err, context.Canceled)
}

func TestHiddenSkill(t *testing.T) {
	_, ss := project(t)
	_, err := run(t, Tool(ss), `{"name":"secret","path":null}`)
	require.ErrorContains(t, err, "unknown skill")

	s, ok := Find(ss, "secret")
	require.True(t, ok)
	out, err := Expand(context.Background(), s, "do it")
	require.NoError(t, err)
	require.Contains(t, out, "Secret body.")
	require.True(t, strings.HasSuffix(out, "\n\ndo it"))

	out, err = Expand(context.Background(), s, "")
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(out, "</skill_content>"))
}

func TestPathEscapes(t *testing.T) {
	root, ss := project(t)
	outside := realDir(t)
	write(t, filepath.Join(outside, "secret.txt"), "top secret")
	write(t, filepath.Join(root, "sibling.txt"), "sibling")
	skill := filepath.Join(root, "skills", "pdf")
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(skill, "link.txt")))
	require.NoError(t, os.Symlink(outside, filepath.Join(skill, "linkdir")))
	tool := Tool(ss)

	for _, p := range []string{"../../../sibling.txt", "..", "/etc/passwd", filepath.Join(outside, "secret.txt"), "link.txt", "linkdir/secret.txt"} {
		in, _ := json.Marshal(map[string]any{"name": "pdf", "path": p})
		out, err := run(t, tool, string(in))
		require.Error(t, err, p)
		require.NotContains(t, out, "secret")
	}
}

func TestUserSourceSymlinkEscape(t *testing.T) {
	d, target, outside := realDir(t), realDir(t), realDir(t)
	write(t, filepath.Join(target, "SKILL.md"), skillMD("linked", "d"))
	write(t, filepath.Join(outside, "secret.txt"), "top secret")
	require.NoError(t, os.Symlink(outside, filepath.Join(target, "out")))
	require.NoError(t, os.Symlink(target, filepath.Join(d, "linked")))
	ss, warns := Discover([]Source{userSrc(d)})
	require.Empty(t, warns)

	tool := Tool(ss)
	_, err := run(t, tool, `{"name":"linked","path":"out/secret.txt"}`)
	require.Error(t, err)
	out, err := run(t, tool, `{"name":"linked","path":null}`)
	require.NoError(t, err)
	require.NotContains(t, out, "secret.txt")
}

func TestSkillDirSwappedToSymlink(t *testing.T) {
	root, ss := project(t)
	outside := realDir(t)
	write(t, filepath.Join(outside, "SKILL.md"), skillMD("pdf", "evil"))
	write(t, filepath.Join(outside, "loot.txt"), "loot")
	skill := filepath.Join(root, "skills", "pdf")
	require.NoError(t, os.RemoveAll(skill))
	require.NoError(t, os.Symlink(outside, skill))

	tool := Tool(ss)
	_, err := run(t, tool, `{"name":"pdf","path":null}`)
	require.Error(t, err)
	_, err = run(t, tool, `{"name":"pdf","path":"loot.txt"}`)
	require.Error(t, err)
}

func TestNonRegularAndBinary(t *testing.T) {
	root, ss := project(t)
	skill := filepath.Join(root, "skills", "pdf")
	require.NoError(t, syscall.Mkfifo(filepath.Join(skill, "fifo"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(skill, "adir"), 0o755))
	write(t, filepath.Join(skill, "bin.dat"), "a\x00b")
	tool := Tool(ss)
	for _, p := range []string{"fifo", "adir", "bin.dat", "missing"} {
		_, err := run(t, tool, fmt.Sprintf(`{"name":"pdf","path":%q}`, p))
		require.Error(t, err, p)
	}
}

func TestTruncation(t *testing.T) {
	root, ss := project(t)
	skill := filepath.Join(root, "skills", "pdf")
	write(t, filepath.Join(skill, "big.txt"), strings.Repeat("é", 100_000))
	write(t, filepath.Join(skill, "huge.txt"), strings.Repeat("a", maxFileBytes+10))
	tool := Tool(ss)

	out, err := run(t, tool, `{"name":"pdf","path":"big.txt"}`)
	require.NoError(t, err)
	require.Less(t, len(out), maxResultBytes+200)
	require.Contains(t, out, "bytes omitted")
	require.Equal(t, strings.ToValidUTF8(out, ""), out)

	out, err = run(t, tool, `{"name":"pdf","path":"huge.txt"}`)
	require.NoError(t, err)
	require.Contains(t, out, "only the first 1 MiB was read")
}

func TestResourceCap(t *testing.T) {
	root, ss := project(t)
	skill := filepath.Join(root, "skills", "pdf")
	for i := range 150 {
		write(t, filepath.Join(skill, "refs", fmt.Sprintf("f%03d.md", i)), "x")
	}
	out, err := run(t, Tool(ss), `{"name":"pdf","path":null}`)
	require.NoError(t, err)
	require.Equal(t, 100, strings.Count(out, "<file>"))
	require.Contains(t, out, "more than 100 files")
}

func TestUnreadableSubdirStillLoads(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	root, ss := project(t)
	locked := filepath.Join(root, "skills", "pdf", "locked")
	write(t, filepath.Join(locked, "f.md"), "x")
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	out, err := run(t, Tool(ss), `{"name":"pdf","path":null}`)
	require.NoError(t, err)
	require.Contains(t, out, "Body of pdf.")
	require.Contains(t, out, "<file>scripts/run.sh</file>")
}
