package hooks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/userfile"
)

const label = "~/.durable/hooks.json"

// loadFile writes body as ~/.durable/<name> under a temp home and returns the loaded file.
func loadFile(t *testing.T, name, body string) userfile.File {
	t.Helper()
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".durable"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".durable", name), []byte(body), 0o600))
	f, err := userfile.Load(home, "hooks")
	require.NoError(t, err)
	return f
}

func hookConfig(handler string) string {
	return `{"hooks":{"PreToolUse":[{"matcher":"bash","hooks":[` + handler + `]}]}}`
}

func TestParseConfigMissingFile(t *testing.T) {
	cfg, warnings, err := ParseConfig(nil, label)
	require.NoError(t, err)
	require.Empty(t, warnings)
	require.True(t, cfg.Empty())
	require.Empty(t, cfg.Counts())
}

func TestParseConfigCountsAndWarnings(t *testing.T) {
	cfg, warnings, err := ParseConfig([]byte(`{"hooks":{
		"PreToolUse":[{"matcher":"bash","hooks":[{"type":"command","command":"a"},{"type":"command","command":"b"}]}],
		"PostToolUse":[{"hooks":[{"type":"command","command":"c","args":["x"],"timeout":5,"onFailure":"continue","useWorkspaceCwd":true}]}],
		"Stop":[{"hooks":[]}]}}`), label)
	require.NoError(t, err)
	require.False(t, cfg.Empty())
	require.Equal(t, map[string]int{PreToolUse: 2, PostToolUse: 1}, cfg.Counts())
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], "Stop")
}

func TestParseConfigEmptyGroupsAreEmpty(t *testing.T) {
	cfg, _, err := ParseConfig([]byte(`{"hooks":{"PreToolUse":[]}}`), label)
	require.NoError(t, err)
	require.True(t, cfg.Empty())
}

func TestParseConfigErrors(t *testing.T) {
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
			_, _, err := ParseConfig([]byte(body), label)
			require.Error(t, err)
		})
	}
}

func TestUnknownMatchers(t *testing.T) {
	tools := []string{"bash", "edit_file", "mcp__s__t"}
	load := func(body string) Config {
		cfg, _, err := ParseConfig([]byte(body), label)
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

func TestParseConfigYAML(t *testing.T) {
	body := "hooks:\n  PreToolUse:\n    - matcher: bash\n      hooks:\n        - type: command\n          command: a\n          args: [x]\n"
	f := loadFile(t, "hooks.yml", body)
	cfg, warnings, err := ParseConfig(f.Data, f.Label)
	require.NoError(t, err)
	require.Empty(t, warnings)
	require.Equal(t, map[string]int{PreToolUse: 1}, cfg.Counts())

	f = loadFile(t, "hooks.yml", body+"          bogus: 1\n")
	_, _, err = ParseConfig(f.Data, f.Label)
	require.Error(t, err)
}

func TestParseConfigYAMLUnquotedArgs(t *testing.T) {
	f := loadFile(t, "hooks.yaml", "hooks:\n  PreToolUse:\n    - hooks:\n        - type: command\n          command: a\n          args: [--port, 8080, true]\n")
	cfg, _, err := ParseConfig(f.Data, f.Label)
	require.NoError(t, err)
	require.Equal(t, []string{"--port", "8080", "true"}, cfg.events[PreToolUse][0].hooks[0].Args)
}

func TestParseConfigRejectsNonScalarArgs(t *testing.T) {
	_, _, err := ParseConfig([]byte(hookConfig(`{"type":"command","command":"a","args":[[1]]}`)), label)
	require.ErrorContains(t, err, "args[0] must be a string, number or boolean")
}

func TestParseConfigSameErrorForJSONAndYAML(t *testing.T) {
	jsonBody := hookConfig(`{"type":"command"}`)
	yamlBody := "hooks:\n  PreToolUse:\n    - matcher: bash\n      hooks:\n        - type: command\n"
	_, _, jerr := ParseConfig(loadFile(t, "hooks.json", jsonBody).Data, "~/.durable/hooks.x")
	_, _, yerr := ParseConfig(loadFile(t, "hooks.yaml", yamlBody).Data, "~/.durable/hooks.x")
	require.EqualError(t, jerr, "PreToolUse[0] in ~/.durable/hooks.x: hooks[0]: command is required")
	require.Equal(t, jerr.Error(), yerr.Error())
}

func TestParseConfigTypeError(t *testing.T) {
	f := loadFile(t, "hooks.yaml", "hooks:\n  PreToolUse:\n    - hooks:\n        - {type: command, command: a, timeout: 0x1F}\n")
	_, _, err := ParseConfig(f.Data, f.Label)
	require.EqualError(t, err, "PreToolUse[0] in ~/.durable/hooks.yaml: hooks[0]: timeout must be a number, got string; in YAML write numbers as plain decimals (no 0x, leading zeros or underscores) and booleans as true or false")
}
