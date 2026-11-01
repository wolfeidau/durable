package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/skills"
)

func sampleSkills(name string) []skills.Skill {
	return []skills.Skill{{Name: name, Description: "d", Root: "/r", Dir: "."}}
}

func discoverSkills(list []skills.Skill, warnings ...string) func() ([]skills.Skill, []string, error) {
	return func() ([]skills.Skill, []string, error) { return list, warnings, nil }
}

func noSkillDiscovery(t *testing.T) func() ([]skills.Skill, []string, error) {
	t.Helper()
	return func() ([]skills.Skill, []string, error) {
		t.Error("discovery must not run")
		return nil, nil, nil
	}
}

func TestPinSkillsFresh(t *testing.T) {
	dir := t.TempDir()
	res, err := pinSkills(dir, false, false, discoverSkills(sampleSkills("a"), "careful"))
	require.NoError(t, err)
	require.Equal(t, sampleSkills("a"), res.Skills)
	require.Equal(t, []string{"careful"}, res.Warnings)
	require.False(t, res.Pinned)

	info, err := os.Stat(filepath.Join(dir, skillsPinFile))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestPinSkillsReusesPinWithoutDiscovery(t *testing.T) {
	dir := t.TempDir()
	_, err := pinSkills(dir, false, false, discoverSkills(sampleSkills("a")))
	require.NoError(t, err)

	res, err := pinSkills(dir, true, false, noSkillDiscovery(t))
	require.NoError(t, err)
	require.Equal(t, sampleSkills("a"), res.Skills)
	require.True(t, res.Pinned)
}

func TestPinSkillsNoneFoundPinsEmptyList(t *testing.T) {
	dir := t.TempDir()
	res, err := pinSkills(dir, false, false, discoverSkills(nil))
	require.NoError(t, err)
	require.Empty(t, res.Skills)
	b, err := os.ReadFile(filepath.Join(dir, skillsPinFile))
	require.NoError(t, err)
	require.JSONEq(t, "[]", string(b))
}

func TestPinSkillsOlderSessionPinsEmpty(t *testing.T) {
	dir := t.TempDir()
	res, err := pinSkills(dir, true, false, noSkillDiscovery(t))
	require.NoError(t, err)
	require.Empty(t, res.Skills)
	require.True(t, res.Pinned)

	res, err = pinSkills(dir, true, false, discoverSkills(sampleSkills("late")))
	require.NoError(t, err)
	require.Empty(t, res.Skills)
}

func TestPinSkillsCorruptPinErrors(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, skillsPinFile), []byte("{nope"), 0o600))
	_, err := pinSkills(dir, false, false, noSkillDiscovery(t))
	require.ErrorContains(t, err, "failed to parse pinned skills")
}

func TestPinSkillsLostRaceUsesWinner(t *testing.T) {
	dir := t.TempDir()
	discover := func() ([]skills.Skill, []string, error) {
		_, err := pinSkills(dir, false, false, discoverSkills(sampleSkills("winner")))
		require.NoError(t, err)
		return sampleSkills("loser"), []string{"stale"}, nil
	}
	res, err := pinSkills(dir, false, false, discover)
	require.NoError(t, err)
	require.Equal(t, sampleSkills("winner"), res.Skills)
	require.Empty(t, res.Warnings)
	require.True(t, res.Pinned)
}

func TestPinSkillsNoContextPinsEmpty(t *testing.T) {
	dir := t.TempDir()
	res, err := pinSkills(dir, false, true, noSkillDiscovery(t))
	require.NoError(t, err)
	require.Empty(t, res.Skills)
	require.False(t, res.Pinned)

	res, err = pinSkills(dir, true, false, noSkillDiscovery(t))
	require.NoError(t, err)
	require.Empty(t, res.Skills)
}

func sourceDirs(srcs []skills.Source) []string {
	var dirs []string
	for _, s := range srcs {
		dirs = append(dirs, s.Dir)
	}
	return dirs
}

func TestSkillSourcesOrder(t *testing.T) {
	repo, home := repoWithHome(t)
	sub := filepath.Join(repo, "sub")
	require.NoError(t, os.Mkdir(sub, 0o755))

	srcs, err := skillSources(sub, home)
	require.NoError(t, err)
	require.Equal(t, []string{
		filepath.Join(sub, ".durable", "skills"),
		filepath.Join(sub, ".agents", "skills"),
		filepath.Join(repo, ".durable", "skills"),
		filepath.Join(repo, ".agents", "skills"),
		filepath.Join(home, ".durable", "skills"),
		filepath.Join(home, ".agents", "skills"),
	}, sourceDirs(srcs))
	for _, s := range srcs[:4] {
		require.Equal(t, repo, s.Root)
	}
	require.Empty(t, srcs[4].Root)
	require.Empty(t, srcs[5].Root)
}

func TestSkillSourcesUnownedGitFallsThrough(t *testing.T) {
	repo, home := repoWithHome(t)
	sub := filepath.Join(repo, "sub")
	require.NoError(t, os.Mkdir(sub, 0o755))

	srcs := skillSourcesFor(sub, home, uint32(os.Geteuid())+1)
	require.Equal(t, []string{
		filepath.Join(sub, ".durable", "skills"),
		filepath.Join(sub, ".agents", "skills"),
		filepath.Join(home, ".durable", "skills"),
		filepath.Join(home, ".agents", "skills"),
	}, sourceDirs(srcs))
	require.Equal(t, sub, srcs[0].Root)
}

func TestExpandSkillLine(t *testing.T) {
	dir := realTempDir(t)
	writeFile(t, filepath.Join(dir, "demo", "SKILL.md"), "---\nname: demo\ndescription: d\n---\n\nDemo body.\n")
	list := []skills.Skill{{Name: "demo", Description: "d", Root: filepath.Join(dir, "demo"), Dir: "."}}

	tests := []struct {
		name    string
		line    string
		ok      bool
		want    []string
		missing string
	}{
		{name: "plain line", line: "hello", ok: true, want: []string{"hello"}},
		{name: "no text", line: "/skill:demo", ok: true, want: []string{"Demo body."}},
		{name: "text", line: "/skill:demo do it", ok: true, want: []string{"Demo body.", "do it"}},
		{name: "extra whitespace", line: "/skill:demo \t  do  it  ", ok: true, want: []string{"Demo body.", "\n\ndo  it"}},
		{name: "unknown", line: "/skill:nope x", missing: `unknown skill "nope"`},
		{name: "empty name", line: "/skill: x", missing: `unknown skill ""`},
		{name: "bare prefix", line: "/skill:", missing: `unknown skill ""`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			r := &repl{skills: list, out: &out}
			prompt, ok := r.expandSkill(tt.line)
			require.Equal(t, tt.ok, ok)
			for _, w := range tt.want {
				require.Contains(t, prompt, w)
			}
			if tt.ok {
				require.Empty(t, out.String())
			} else {
				require.Empty(t, prompt)
				require.Contains(t, out.String(), tt.missing)
			}
		})
	}
}

func TestExpandSkillLineRemovedSkill(t *testing.T) {
	var out bytes.Buffer
	r := &repl{skills: []skills.Skill{{Name: "gone", Description: "d", Root: realTempDir(t), Dir: "."}}, out: &out}
	_, ok := r.expandSkill("/skill:gone")
	require.False(t, ok)
	require.Contains(t, out.String(), `failed to load skill "gone"`)
}
