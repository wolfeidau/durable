package hooks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hooks.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func hookConfig(handler string) string {
	return `{"hooks":{"PreToolUse":[{"matcher":"bash","hooks":[` + handler + `]}]}}`
}

func TestLoadConfigMissingFile(t *testing.T) {
	cfg, warnings, err := LoadConfig(filepath.Join(t.TempDir(), "absent.json"))
	require.NoError(t, err)
	require.Empty(t, warnings)
	require.True(t, cfg.Empty())
	require.Empty(t, cfg.Counts())
}

func TestLoadConfigCountsAndWarnings(t *testing.T) {
	cfg, warnings, err := LoadConfig(writeConfig(t, `{"hooks":{
		"PreToolUse":[{"matcher":"bash","hooks":[{"type":"command","command":"a"},{"type":"command","command":"b"}]}],
		"PostToolUse":[{"hooks":[{"type":"command","command":"c","args":["x"],"timeout":5,"onFailure":"continue","useWorkspaceCwd":true}]}],
		"Stop":[{"hooks":[]}]}}`))
	require.NoError(t, err)
	require.False(t, cfg.Empty())
	require.Equal(t, map[string]int{PreToolUse: 2, PostToolUse: 1}, cfg.Counts())
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], "Stop")
}

func TestLoadConfigEmptyGroupsAreEmpty(t *testing.T) {
	cfg, _, err := LoadConfig(writeConfig(t, `{"hooks":{"PreToolUse":[]}}`))
	require.NoError(t, err)
	require.True(t, cfg.Empty())
}

func TestLoadConfigErrors(t *testing.T) {
	for name, body := range map[string]string{
		"invalid json":      `{"hooks":`,
		"unknown field":     hookConfig(`{"type":"command","command":"a","if":"x"}`),
		"unknown group key": `{"hooks":{"PreToolUse":[{"matcher":"","extra":1,"hooks":[]}]}}`,
		"http type":         hookConfig(`{"type":"http","command":"a"}`),
		"missing type":      hookConfig(`{"command":"a"}`),
		"missing command":   hookConfig(`{"type":"command"}`),
		"negative timeout":  hookConfig(`{"type":"command","command":"a","timeout":-1}`),
		"bad onFailure":     hookConfig(`{"type":"command","command":"a","onFailure":"warn"}`),
		"bad regexp":        `{"hooks":{"PreToolUse":[{"matcher":"mcp__(","hooks":[]}]}}`,
		"groups not array":  `{"hooks":{"PreToolUse":{}}}`,
		"misspelt top key":  `{"hook":{}}`,
		"misspelt event":    `{"hooks":{"preToolUse":[]}}`,
		"misspelt post":     `{"hooks":{"POSTTOOLUSE":[]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := LoadConfig(writeConfig(t, body))
			require.Error(t, err)
		})
	}
}

func TestUnknownMatchers(t *testing.T) {
	tools := []string{"bash", "edit_file", "mcp__s__t"}
	load := func(body string) Config {
		cfg, _, err := LoadConfig(writeConfig(t, body))
		require.NoError(t, err)
		return cfg
	}
	group := func(event, matcher string) string {
		return `"` + event + `":[{"matcher":"` + matcher + `","hooks":[{"type":"command","command":"a"}]}]`
	}

	require.Empty(t, load(`{"hooks":{`+group("PreToolUse", "bash")+`}}`).UnknownMatchers(tools))
	require.Empty(t, Config{}.UnknownMatchers(tools))

	require.Equal(t,
		[]string{`PreToolUse matcher "Bash" matches no tool`},
		load(`{"hooks":{`+group("PreToolUse", "Bash")+`}}`).UnknownMatchers(tools))

	require.Equal(t,
		[]string{`PreToolUse matcher "^Bash$" matches no tool in this session`},
		load(`{"hooks":{`+group("PreToolUse", "^Bash$")+`}}`).UnknownMatchers(tools))
	require.Empty(t, load(`{"hooks":{`+group("PreToolUse", "^mcp__.*")+`}}`).UnknownMatchers(tools))

	require.Equal(t,
		[]string{`PostToolUse matcher "Edit" matches no tool`},
		load(`{"hooks":{`+group("PostToolUse", "bash|Edit")+`}}`).UnknownMatchers(tools),
		"only the unknown name warns")

	require.Equal(t,
		[]string{`PreToolUse matcher "Bash" matches no tool`, `PostToolUse matcher "Write" matches no tool`},
		load(`{"hooks":{`+group("PostToolUse", "Write")+`,`+group("PreToolUse", "Bash")+`}}`).UnknownMatchers(tools),
		"each warning keeps its event")

	for _, m := range []string{"", "*", "ba.*", `^(bash)$`} {
		require.Empty(t, load(`{"hooks":{`+group("PreToolUse", m)+`}}`).UnknownMatchers(tools), "matcher %q matches a tool", m)
	}
}

func TestMatcher(t *testing.T) {
	for _, tc := range []struct {
		matcher string
		match   []string
		miss    []string
	}{
		{"", []string{"bash", "x"}, nil},
		{"*", []string{"bash", "x"}, nil},
		{"bash", []string{"bash"}, []string{"bash2", "xbash", ""}},
		{"bash|edit_file", []string{"bash", "edit_file"}, []string{"grep"}},
		{" bash , edit_file ", []string{"bash", "edit_file"}, []string{"grep", " bash"}},
		{"mcp__.*", []string{"mcp__s__t"}, []string{"bash"}},
		{"^(bash|grep)$", []string{"bash", "grep"}, []string{"bash2"}},
		{"mcp__github__.+", []string{"mcp__github__x"}, []string{"mcp__other__x"}},
	} {
		m, err := parseMatcher(tc.matcher)
		require.NoError(t, err, tc.matcher)
		for _, n := range tc.match {
			require.True(t, m.matches(n), "%q should match %q", tc.matcher, n)
		}
		for _, n := range tc.miss {
			require.False(t, m.matches(n), "%q should not match %q", tc.matcher, n)
		}
	}
}
