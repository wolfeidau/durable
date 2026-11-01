package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func skillMD(name, desc string) string {
	return fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\nBody of %s.\n", name, desc, name)
}

func realDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return d
}

func userSrc(dir string) Source { return Source{Dir: dir} }

func TestParse(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		ok     bool
		want   Skill
		warned string
	}{
		{name: "valid", text: skillMD("pdf", "Do PDFs."), ok: true, want: Skill{Name: "pdf", Description: "Do PDFs."}},
		{name: "empty file", text: "", warned: "no frontmatter"},
		{name: "no closing delimiter", text: "---\nname: pdf\ndescription: x\n", warned: "no frontmatter"},
		{name: "no opening", text: "name: pdf\n", warned: "no frontmatter"},
		{name: "unquoted colon", text: "---\nname: pdf\ndescription: Use when: PDFs appear\n---\nb", ok: true, want: Skill{Name: "pdf", Description: "Use when: PDFs appear"}, warned: "invalid YAML"},
		{name: "hidden survives invalid YAML", text: "---\nname: pdf\ndescription: Use when: PDFs\ndisable-model-invocation: \"true\"\n---\nb", ok: true, want: Skill{Name: "pdf", Description: "Use when: PDFs", Hidden: true}, warned: "invalid YAML"},
		{name: "missing description", text: "---\nname: pdf\n---\nb", warned: "no description"},
		{name: "empty description", text: "---\nname: pdf\ndescription: \"  \"\n---\nb", warned: "no description"},
		{name: "non-string description", text: "---\nname: pdf\ndescription: 42\n---\nb", warned: "no description"},
		{name: "non-string name", text: "---\nname: 42\ndescription: d\n---\nb", ok: true, want: Skill{Name: "pdf", Description: "d"}, warned: "no name"},
		{name: "absent name", text: "---\ndescription: d\n---\nb", ok: true, want: Skill{Name: "pdf", Description: "d"}, warned: "no name"},
		{name: "BOM and CRLF", text: "\ufeff---\r\nname: pdf\r\ndescription: d\r\n---\r\nb", ok: true, want: Skill{Name: "pdf", Description: "d"}},
		{name: "hidden", text: "---\nname: pdf\ndescription: d\ndisable-model-invocation: true\n---\nb", ok: true, want: Skill{Name: "pdf", Description: "d", Hidden: true}},
		{name: "hidden non-bool", text: "---\nname: pdf\ndescription: d\ndisable-model-invocation: \"yes\"\n---\nb", ok: true, want: Skill{Name: "pdf", Description: "d"}},
		{name: "name mismatch", text: skillMD("other", "d"), ok: true, want: Skill{Name: "other", Description: "d"}, warned: "does not match"},
		{name: "invalid name", text: skillMD("Bad_Name", "d"), ok: true, want: Skill{Name: "Bad_Name", Description: "d"}, warned: "not valid"},
		{name: "long name", text: skillMD(strings.Repeat("a", 65), "d"), ok: true, want: Skill{Name: strings.Repeat("a", 65), Description: "d"}, warned: "not valid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, warns, ok := parse(tt.text, "pdf", "/x/pdf")
			require.Equal(t, tt.ok, ok)
			if tt.ok {
				require.Equal(t, tt.want, s)
			}
			if tt.warned == "" {
				require.Empty(t, warns)
			} else {
				require.Contains(t, strings.Join(warns, "\n"), tt.warned)
			}
		})
	}
}

func TestDescriptionCap(t *testing.T) {
	// 3-byte runes put the 1024 byte boundary mid-rune.
	desc := strings.Repeat("€", 400)
	s, warns, ok := parse(skillMD("pdf", desc), "pdf", "/x")
	require.True(t, ok)
	require.LessOrEqual(t, len(s.Description), maxDescription)
	require.True(t, strings.HasPrefix(desc, s.Description))
	require.Len(t, s.Description, 1023)
	require.Contains(t, strings.Join(warns, "\n"), "description cut")
}

func TestDiscoverPrecedenceAndShadow(t *testing.T) {
	a, b := realDir(t), realDir(t)
	write(t, filepath.Join(a, "pdf", "SKILL.md"), skillMD("pdf", "from a"))
	write(t, filepath.Join(b, "pdf", "SKILL.md"), skillMD("pdf", "from b"))
	write(t, filepath.Join(b, "alpha", "SKILL.md"), skillMD("alpha", "alpha"))
	write(t, filepath.Join(b, "not-a-skill", "README.md"), "x")

	got, warns := Discover([]Source{userSrc(a), userSrc(filepath.Join(b, "missing")), userSrc(b)})
	require.Len(t, got, 2)
	require.Equal(t, "alpha", got[0].Name)
	require.Equal(t, "from a", got[1].Description)
	require.Len(t, warns, 1)
	require.Contains(t, warns[0], "shadowed")
	require.Contains(t, warns[0], filepath.Join(b, "pdf"))
	require.Contains(t, warns[0], filepath.Join(a, "pdf"))
}

func TestDiscoverCap(t *testing.T) {
	d := realDir(t)
	for i := range 70 {
		n := fmt.Sprintf("s%02d", i)
		write(t, filepath.Join(d, n, "SKILL.md"), skillMD(n, "d"))
	}
	for i := range 3 {
		n := fmt.Sprintf("h%d", i)
		write(t, filepath.Join(d, n, "SKILL.md"), "---\nname: "+n+"\ndescription: d\ndisable-model-invocation: true\n---\n")
	}
	got, warns := Discover([]Source{userSrc(d)})
	visible, hidden := 0, 0
	for _, s := range got {
		if s.Hidden {
			hidden++
		} else {
			visible++
		}
	}
	require.Equal(t, 64, visible)
	require.Equal(t, 3, hidden)
	require.Len(t, warns, 6)
	require.Contains(t, warns[0], "s64")
}

func TestDiscoverUserSymlink(t *testing.T) {
	d, target := realDir(t), realDir(t)
	write(t, filepath.Join(target, "SKILL.md"), skillMD("linked", "d"))
	require.NoError(t, os.Symlink(target, filepath.Join(d, "linked")))
	got, warns := Discover([]Source{userSrc(d)})
	require.Empty(t, warns)
	require.Equal(t, []Skill{{Name: "linked", Description: "d", Root: target, Dir: "."}}, got)
}

func TestDiscoverProject(t *testing.T) {
	root := realDir(t)
	dir := filepath.Join(root, ".agents", "skills")
	write(t, filepath.Join(dir, "pdf", "SKILL.md"), skillMD("pdf", "d"))
	got, warns := Discover([]Source{{Dir: dir, Root: root}})
	require.Empty(t, warns)
	require.Equal(t, []Skill{{Name: "pdf", Description: "d", Root: root, Dir: filepath.Join(".agents", "skills", "pdf")}}, got)
}

func TestDiscoverProjectSymlinkEscape(t *testing.T) {
	root, outside := realDir(t), realDir(t)
	dir := filepath.Join(root, "skills")
	write(t, filepath.Join(outside, "SKILL.md"), skillMD("evil", "d"))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "evil")))
	// A SKILL.md that is itself a link out.
	write(t, filepath.Join(outside, "other.md"), skillMD("sneaky", "d"))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sneaky"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(outside, "other.md"), filepath.Join(dir, "sneaky", "SKILL.md")))

	got, warns := Discover([]Source{{Dir: dir, Root: root}})
	require.Empty(t, got)
	require.Len(t, warns, 2)
}

func TestDiscoverRejectsBadSkillFiles(t *testing.T) {
	d := realDir(t)
	write(t, filepath.Join(d, "nul", "SKILL.md"), "---\nname: nul\ndescription: d\n---\n\x00")
	require.NoError(t, os.MkdirAll(filepath.Join(d, "dir", "SKILL.md"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(d, "fifo"), 0o755))
	require.NoError(t, syscall.Mkfifo(filepath.Join(d, "fifo", "SKILL.md"), 0o644))
	got, warns := Discover([]Source{userSrc(d)})
	require.Empty(t, got)
	require.Len(t, warns, 3)
}

func TestSkillJSONRoundTrip(t *testing.T) {
	in := []Skill{{Name: "a", Description: "d", Root: "/r", Dir: ".", Hidden: true}, {Name: "b", Description: "e", Root: "/r", Dir: "x/b"}}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	var out []Skill
	require.NoError(t, json.Unmarshal(b, &out))
	require.Equal(t, in, out)
}
