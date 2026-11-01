package main

import (
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/hooks"
)

// hookScript writes an executable bash script outside the workspace and returns its path.
func hookScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook.sh")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/bash\n"+body+"\n"), 0o700))
	return path
}

func hookHandler(command string, extra map[string]any) map[string]any {
	h := map[string]any{"type": "command", "command": command}
	maps.Copy(h, extra)
	return h
}

// writeHooks writes ~/.durable/hooks.json with one matcher group per event.
func writeHooks(t *testing.T, home string, events map[string][]map[string]any) {
	t.Helper()
	cfg := map[string][]any{}
	for event, handlers := range events {
		cfg[event] = []any{map[string]any{"matcher": "bash", "hooks": handlers}}
	}
	writeHooksRaw(t, filepath.Join(home, ".durable", "hooks.json"), mustJSON(t, map[string]any{"hooks": cfg}))
}

func writeHooksRaw(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// sendPrompt writes one prompt and waits until the API has seen want requests.
func sendPrompt(t *testing.T, api *fakeAPI, want int, a *agent, prompt string) {
	t.Helper()
	_, err := io.WriteString(a.stdin, prompt+"\n")
	require.NoError(t, err)
	eventually(t, "API requests", func() bool { return len(api.requests()) == want })
}

func finish(t *testing.T, a *agent) {
	t.Helper()
	require.NoError(t, a.stdin.Close())
	require.NoError(t, waitExit(t, a), a.out.String())
}

func TestHookDenyStopsBashAndReasonReachesModel(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	marker := filepath.Join(ws, "marker.txt")
	writeHooks(t, home, map[string][]map[string]any{
		hooks.PreToolUse: {hookHandler(hookScript(t, `echo "policy-denied-xyz" >&2; exit 2`), nil)},
	})
	api := newFakeAPI(t, "echo ran >> marker.txt")

	// No --yes: the hook denies before the approval prompt.
	a := startAgent(t, bin, ws, home, api.srv.URL)
	runPrompt(t, api, 2, a, "go")

	require.NoFileExists(t, marker)
	require.NotContains(t, a.out.String(), "[y/N]")
	require.Contains(t, a.out.String(), "[hooks] PreToolUse: 1 configured")
	require.True(t, hasToolResult(t, api.requests()[1], true, "policy-denied-xyz"), api.requests()[1])
}

func TestHookTimeoutIsKilledAndDeniedThenSessionContinues(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	marker := filepath.Join(ws, "marker.txt")
	pidFile := filepath.Join(t.TempDir(), "pid")
	hang := hookScript(t, `echo $$ > `+pidFile+`; sleep 60`)
	writeHooks(t, home, map[string][]map[string]any{
		hooks.PreToolUse: {hookHandler(hang, map[string]any{"timeout": 1})},
	})
	api := newFakeAPI(t, "echo ran >> marker.txt")

	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	sendPrompt(t, api, 2, a, "first")
	require.NoFileExists(t, marker)
	require.True(t, hasToolResult(t, api.requests()[1], true, hooks.Blocked), api.requests()[1])

	pid, err := strconv.Atoi(strings.TrimSpace(string(mustRead(t, pidFile))))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH }, testTimeout, 20*time.Millisecond, "the hook must be killed")

	sendPrompt(t, api, 3, a, "second")
	finish(t, a)
	hist := finalHistory(t, ws)
	require.Equal(t, "all done", hist[len(hist)-1].Text)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b
}

func TestHookUpdatedInputRunsRewrittenCommand(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	rewrite := hookScript(t, `echo '{"hookSpecificOutput":{"updatedInput":{"command":"echo rewritten >> marker.txt"}}}'`)
	writeHooks(t, home, map[string][]map[string]any{hooks.PreToolUse: {hookHandler(rewrite, nil)}})
	api := newFakeAPI(t, "echo original >> marker.txt")

	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	runPrompt(t, api, 2, a, "go")

	require.Equal(t, []string{"rewritten"}, readLinesOf(t, filepath.Join(ws, "marker.txt")))
	require.True(t, hasToolResult(t, api.requests()[1], false, "A policy changed this call"), api.requests()[1])
}

func TestHooksApplyUnderYes(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"deny": {`echo "no-way" >&2; exit 2`, "no-way"},
		"ask":  {`echo '{"hookSpecificOutput":{"permissionDecision":"ask"}}'`, "A policy blocked this tool call"},
	} {
		t.Run(name, func(t *testing.T) {
			bin := buildBinary(t)
			ws, home := newWorkspace(t)
			writeHooks(t, home, map[string][]map[string]any{hooks.PreToolUse: {hookHandler(hookScript(t, tc.body), nil)}})
			api := newFakeAPI(t, "echo ran >> marker.txt")

			a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
			runPrompt(t, api, 2, a, "go")

			require.NoFileExists(t, filepath.Join(ws, "marker.txt"))
			require.NotContains(t, a.out.String(), "[y/N]", "--yes never prompts")
			require.True(t, hasToolResult(t, api.requests()[1], true, tc.want), api.requests()[1])
		})
	}
}

func TestHookPayloadCarriesSessionAndMode(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		mode string
	}{
		"default": {nil, "default"},
		"yes":     {[]string{"--yes"}, "bypassPermissions"},
	} {
		t.Run(name, func(t *testing.T) {
			bin := buildBinary(t)
			ws, home := newWorkspace(t)
			capture := filepath.Join(t.TempDir(), "stdin.json")
			// exit 0 with an allow, so the call also runs without a prompt.
			body := `cat > ` + capture + `; echo '{"hookSpecificOutput":{"permissionDecision":"allow"}}'`
			writeHooks(t, home, map[string][]map[string]any{hooks.PreToolUse: {hookHandler(hookScript(t, body), nil)}})
			api := newFakeAPI(t, "true")

			a := startAgent(t, bin, ws, home, api.srv.URL, tc.args...)
			runPrompt(t, api, 2, a, "go")

			var p struct {
				Protocol  int    `json:"hook_protocol"`
				SessionID string `json:"session_id"`
				CWD       string `json:"cwd"`
				Mode      string `json:"permission_mode"`
				Event     string `json:"hook_event_name"`
				Tool      string `json:"tool_name"`
			}
			require.NoError(t, json.Unmarshal(mustRead(t, capture), &p))
			require.Equal(t, 1, p.Protocol)
			require.Equal(t, filepath.Base(sessionDir(t, ws)), p.SessionID)
			require.Equal(t, ws, p.CWD)
			require.Equal(t, tc.mode, p.Mode)
			require.Equal(t, "PreToolUse", p.Event)
			require.Equal(t, "bash", p.Tool)
		})
	}
}

const redactMarker = "SECRETMARKER-123"

// sessionBytes returns the database and its WAL and journal side files, which together hold every committed byte.
func sessionBytes(t *testing.T, ws string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		path := filepath.Join(sessionDir(t, ws), "session.sqlite"+suffix)
		if b, err := os.ReadFile(path); err == nil {
			files[filepath.Base(path)] = b
		}
	}
	require.Contains(t, files, "session.sqlite")
	return files
}

func TestPostToolUseRedactsRequestAndDatabase(t *testing.T) {
	// The command prints the marker without containing it, so only the output can leak it.
	for name, command := range map[string]string{
		"success": "printf 'SECRET''MARKER-123'",
		"failure": "printf 'SECRET''MARKER-123'; exit 1",
	} {
		t.Run(name, func(t *testing.T) {
			bin := buildBinary(t)
			ws, home := newWorkspace(t)
			seen := filepath.Join(t.TempDir(), "seen.json")
			body := `cat > ` + seen + `; echo '{"hookSpecificOutput":{"updatedToolOutput":"redacted-by-hook"}}'`
			writeHooks(t, home, map[string][]map[string]any{hooks.PostToolUse: {hookHandler(hookScript(t, body), nil)}})
			api := newFakeAPI(t, command)

			a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
			sendPrompt(t, api, 2, a, "go")
			require.Contains(t, string(mustRead(t, seen)), redactMarker, "the hook must see the raw output")

			reqs := api.requests()
			require.NotContains(t, reqs[1], redactMarker)
			require.True(t, hasToolResult(t, reqs[1], name == "failure", "redacted-by-hook"), reqs[1])

			// Mid-run, so a marker still in the WAL would show.
			for file, b := range sessionBytes(t, ws) {
				t.Logf("mid-run session file %s: %d bytes", file, len(b))
				require.NotContains(t, string(b), redactMarker, file)
			}
			finish(t, a)
			files := sessionBytes(t, ws)
			t.Logf("session files after exit: %v", len(files))
			var all []byte
			for file, b := range files {
				require.NotContains(t, string(b), redactMarker, file)
				all = append(all, b...)
			}
			require.Contains(t, string(all), "redacted-by-hook", "the redacted text is what was stored")
			for _, r := range api.requests() {
				require.NotContains(t, r, redactMarker)
			}
		})
	}
}

func TestKillDuringPostToolUseHookRecordsUnknownOutcome(t *testing.T) {
	// kill -9 leaves the hook's temp directory behind.
	t.Setenv("TMPDIR", t.TempDir())
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	marker := filepath.Join(ws, "marker.txt")
	hookMarker := filepath.Join(t.TempDir(), "hook.txt")
	slow := hookScript(t, `echo ran >> `+hookMarker+`; sleep 5`)
	writeHooks(t, home, map[string][]map[string]any{hooks.PostToolUse: {hookHandler(slow, nil)}})
	api := newFakeAPI(t, "echo ran >> marker.txt")

	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	_, err := io.WriteString(a.stdin, "go\n")
	require.NoError(t, err)
	eventually(t, "hook start", func() bool { return len(readLinesOf(t, hookMarker)) == 1 })
	a.kill(t)

	restart(t, bin, ws, home, api.srv.URL, "--yes")

	require.Len(t, readLinesOf(t, marker), 1, "bash must not run twice")
	require.Len(t, readLinesOf(t, hookMarker), 1, "the hook must not run again on resume")
	reqs := api.requests()
	require.Len(t, reqs, 2)
	require.True(t, hasToolResult(t, reqs[1], true, durable.UnknownOutcome), reqs[1])
	hist := finalHistory(t, ws)
	require.Equal(t, []durable.Role{durable.RoleUser, durable.RoleAssistant, durable.RoleTool, durable.RoleAssistant}, historyRoles(hist))
}

func TestRepositoryHooksFileIsIgnored(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	deny := mustJSON(t, map[string]any{"hooks": map[string]any{hooks.PreToolUse: []any{
		map[string]any{"hooks": []any{hookHandler(hookScript(t, `exit 2`), nil)}},
	}}})
	writeHooksRaw(t, filepath.Join(ws, ".durable", "hooks.json"), deny)
	api := newFakeAPI(t, "echo ran >> marker.txt")

	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	runPrompt(t, api, 2, a, "go")

	require.Len(t, readLinesOf(t, filepath.Join(ws, "marker.txt")), 1)
	require.NotContains(t, a.out.String(), "[hooks]")
}

var pinFileNames = []string{"instructions.md", "mcp-tools.json", "provider.json", "skills.json"}

func TestInvalidHooksConfigRefusesNewSession(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	writeHooksRaw(t, filepath.Join(home, ".durable", "hooks.json"), `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"x","if":"y"}]}]}}`)
	api := newFakeAPI(t, "echo ran >> marker.txt")

	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	require.NoError(t, a.stdin.Close())
	require.Error(t, waitExit(t, a), a.out.String())
	require.Contains(t, a.out.String(), "hooks")

	for _, pin := range pinFileNames {
		require.NoFileExists(t, filepath.Join(sessionDir(t, ws), pin))
	}
	require.NoFileExists(t, filepath.Join(sessionDir(t, ws), "session.sqlite"))
	require.Empty(t, api.requests())
}

func TestInvalidHooksConfigRefusesResume(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	marker := filepath.Join(ws, "marker.txt")
	api := newFakeAPI(t, "echo ran >> marker.txt")

	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	runPrompt(t, api, 2, a, "first")
	before := mustReadDir(t, sessionDir(t, ws))
	require.Len(t, readLinesOf(t, marker), 1)

	writeHooksRaw(t, filepath.Join(home, ".durable", "hooks.json"), `{"hook":{}}`)
	b := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	require.NoError(t, b.stdin.Close())
	require.Error(t, waitExit(t, b), b.out.String())
	require.Contains(t, b.out.String(), "failed to parse hooks config")

	require.Equal(t, before, mustReadDir(t, sessionDir(t, ws)), "nothing new is written")
	require.Len(t, api.requests(), 2)
}

func TestHooksDoNotSeeCredentials(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-openrouter-test")
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "op-token-test")
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	envFile := filepath.Join(t.TempDir(), "env.txt")
	writeHooks(t, home, map[string][]map[string]any{hooks.PreToolUse: {hookHandler(hookScript(t, `env > `+envFile), nil)}})
	api := newFakeAPI(t, "true")

	// startAgent also sets ANTHROPIC_API_KEY=dummy.
	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	runPrompt(t, api, 2, a, "go")

	env := string(mustRead(t, envFile))
	require.Contains(t, env, "PATH=", "the hook ran with an environment")
	for _, prefix := range []string{"ANTHROPIC_", "OPENROUTER_", "OP_"} {
		for line := range strings.SplitSeq(env, "\n") {
			require.False(t, strings.HasPrefix(line, prefix), "hook saw %s", line)
		}
	}
}

func TestUnknownMatcherNameWarns(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	body := mustJSON(t, map[string]any{"hooks": map[string]any{hooks.PreToolUse: []any{
		map[string]any{"matcher": "Bash", "hooks": []any{hookHandler(hookScript(t, `exit 2`), nil)}},
	}}})
	writeHooksRaw(t, filepath.Join(home, ".durable", "hooks.json"), body)
	api := newFakeAPI(t, "echo ran >> marker.txt")

	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	runPrompt(t, api, 2, a, "go")

	require.Contains(t, a.out.String(), `[hooks] warning: PreToolUse matcher "Bash" matches no tool`)
	require.Len(t, readLinesOf(t, filepath.Join(ws, "marker.txt")), 1, "the misnamed policy never fired")
}
