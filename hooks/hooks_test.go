package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
)

// script writes an executable bash script and returns its path.
func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook.sh")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/bash\n"+body+"\n"), 0o700))
	return path
}

func handlerJSON(t *testing.T, command string, extra map[string]any) string {
	t.Helper()
	h := map[string]any{"type": "command", "command": command}
	maps.Copy(h, extra)
	b, err := json.Marshal(h)
	require.NoError(t, err)
	return string(b)
}

// load builds a Config with one matcher group for event from handler JSON objects.
func load(t *testing.T, event, matcher string, handlers ...string) Config {
	t.Helper()
	body := fmt.Sprintf(`{"hooks":{%q:[{"matcher":%q,"hooks":[%s]}]}}`, event, matcher, strings.Join(handlers, ","))
	cfg, _, err := LoadConfig(writeConfig(t, body))
	require.NoError(t, err)
	return cfg
}

func newRunner(cfg Config) *Runner {
	return New(cfg, Options{SessionID: "s1", Workspace: "/work", PermissionMode: "default"})
}

func gate(t *testing.T, r *Runner, base durable.ToolGate, call durable.ToolCall) durable.Decision {
	t.Helper()
	d, err := r.Gate(base)(context.Background(), call)
	require.NoError(t, err)
	return d
}

var bashCall = durable.ToolCall{ID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}

func preRunner(t *testing.T, body string) *Runner {
	t.Helper()
	return newRunner(load(t, PreToolUse, "bash", handlerJSON(t, script(t, body), nil)))
}

// loggedRunner is preRunner with a LogDir, which it returns for reading hooks.log.
func loggedRunner(t *testing.T, body string) (*Runner, string) {
	t.Helper()
	logDir := t.TempDir()
	return New(load(t, PreToolUse, "bash", handlerJSON(t, script(t, body), nil)), Options{LogDir: logDir}), logDir
}

func readLog(t *testing.T, logDir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(logDir, "hooks.log"))
	require.NoError(t, err)
	return string(b)
}

// requireBlocked checks a failure denies with the constant reason and logs the cause.
func requireBlocked(t *testing.T, d durable.Decision, logDir, cause string) {
	t.Helper()
	require.Equal(t, durable.ActionDeny, d.Action)
	require.Equal(t, Blocked, d.Reason)
	require.Contains(t, readLog(t, logDir), cause)
}

func TestPreExitCodes(t *testing.T) {
	for name, tc := range map[string]struct {
		body   string
		action durable.Action
		reason string
		cause  string // non-empty means a failure: Blocked, with this text in hooks.log
	}{
		"empty stdout":     {`exit 0`, durable.ActionAllow, "", ""},
		"non json stdout":  {`echo hello`, durable.ActionAllow, "", ""},
		"exit 2 denies":    {`echo "no rm" >&2; exit 2`, durable.ActionDeny, "no rm", ""},
		"exit 2 no stderr": {`exit 2`, durable.ActionDeny, "", ""},
		"exit 1 fails":     {`echo secret >&2; exit 1`, durable.ActionDeny, Blocked, "exit status 1"},
		"exit 3 fails":     {`exit 3`, durable.ActionDeny, Blocked, "exit status 3"},
		"bad json fails":   {`echo '{nope'`, durable.ActionDeny, Blocked, "invalid hook output"},
		"killed fails":     {`kill -9 $$`, durable.ActionDeny, Blocked, "failed"},
	} {
		t.Run(name, func(t *testing.T) {
			r, logDir := loggedRunner(t, tc.body)
			d := gate(t, r, nil, bashCall)
			require.Equal(t, tc.action, d.Action)
			require.Equal(t, tc.reason, d.Reason)
			if tc.cause != "" {
				requireBlocked(t, d, logDir, tc.cause)
			}
		})
	}
}

func TestPreFailureLogsStderrNotReason(t *testing.T) {
	r, logDir := loggedRunner(t, `echo secret >&2; exit 1`)
	d := gate(t, r, nil, bashCall)
	require.NotContains(t, d.Reason, "secret")
	require.Contains(t, readLog(t, logDir), "secret")
}

func TestPreDecisions(t *testing.T) {
	out := func(s string) string { return `echo '{"hookSpecificOutput":{` + s + `}}'` }
	for name, tc := range map[string]struct {
		body   string
		action durable.Action
		reason string
		cause  string // non-empty means a failure: Blocked, with this text in hooks.log
	}{
		"allow":          {out(`"permissionDecision":"allow"`), durable.ActionAllow, "", ""},
		"ask":            {out(`"permissionDecision":"ask"`), durable.ActionAsk, "", ""},
		"deny reason":    {out(`"permissionDecision":"deny","permissionDecisionReason":"policy"`), durable.ActionDeny, "policy", ""},
		"deny no reason": {out(`"permissionDecision":"deny"`), durable.ActionDeny, "", ""},
		"defer fails":    {out(`"permissionDecision":"defer"`), durable.ActionDeny, Blocked, "defer"},
		"unknown fails":  {out(`"permissionDecision":"maybe"`), durable.ActionDeny, Blocked, "maybe"},
		"no decision":    {out(`"additionalContext":"ignored"`), durable.ActionAllow, "", ""},
		"other fields":   {`echo '{"continue":true}'`, durable.ActionAllow, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			r, logDir := loggedRunner(t, tc.body)
			d := gate(t, r, nil, bashCall)
			require.Equal(t, tc.action, d.Action)
			require.Equal(t, tc.reason, d.Reason)
			if tc.cause != "" {
				requireBlocked(t, d, logDir, tc.cause)
			}
		})
	}
}

func TestPreTopLevelDecisionFails(t *testing.T) {
	for name, body := range map[string]string{
		"block":   `echo '{"decision":"block","reason":"x"}'`,
		"approve": `echo '{"decision":"approve"}'`,
	} {
		t.Run(name, func(t *testing.T) {
			r, logDir := loggedRunner(t, body)
			requireBlocked(t, gate(t, r, nil, bashCall), logDir, "top-level decision")
		})
	}
}

func TestPreUpdatedInputMustBeObject(t *testing.T) {
	for _, in := range []string{`"x"`, `[1]`, `1`, `null`} {
		r, logDir := loggedRunner(t, `echo '{"hookSpecificOutput":{"updatedInput":`+in+`}}'`)
		requireBlocked(t, gate(t, r, nil, bashCall), logDir, "updatedInput")
	}
}

func TestHookCommandIsNotLeakedToModel(t *testing.T) {
	const marker = "MARKER-s3cret-token"
	logDir := t.TempDir()

	argv := handlerJSON(t, "false", map[string]any{"args": []string{marker}})
	pre := New(load(t, PreToolUse, "", argv), Options{LogDir: logDir})
	require.NotContains(t, gate(t, pre, nil, bashCall).Reason, marker)

	inline := handlerJSON(t, "exit 1 # "+marker, nil)
	pre = New(load(t, PreToolUse, "", inline), Options{LogDir: logDir})
	d := gate(t, pre, nil, bashCall)
	require.NotContains(t, d.Reason, marker)
	require.Contains(t, readLog(t, logDir), marker, "the cause stays in the log")

	post := New(load(t, PostToolUse, "", inline), Options{LogDir: logDir})
	out, err := postRun(t, post, fakeTool{name: "bash", out: "original"})
	require.EqualError(t, err, Withheld)
	require.NotContains(t, out, marker)
	require.NotContains(t, err.Error(), marker)
}

func TestPreSkipsNonMatchingTool(t *testing.T) {
	r := preRunner(t, `exit 2`)
	d := gate(t, r, nil, durable.ToolCall{ID: "c", Name: "grep", Input: json.RawMessage(`{}`)})
	require.Equal(t, durable.ActionAllow, d.Action)
}

func TestPrePayload(t *testing.T) {
	for name, input := range map[string]json.RawMessage{
		"object": json.RawMessage(`{"command":"ls"}`),
		"nil":    nil,
		"empty":  json.RawMessage(``),
		"null":   json.RawMessage(`null`),
	} {
		t.Run(name, func(t *testing.T) {
			capture := filepath.Join(t.TempDir(), "stdin.json")
			r := preRunner(t, fmt.Sprintf(`cat > %q`, capture))
			gate(t, r, nil, durable.ToolCall{ID: "id9", Name: "bash", Input: input})

			b, err := os.ReadFile(capture)
			require.NoError(t, err)
			var p map[string]any
			require.NoError(t, json.Unmarshal(b, &p))
			require.EqualValues(t, 1, p["hook_protocol"])
			require.Equal(t, "s1", p["session_id"])
			require.Equal(t, "/work", p["cwd"])
			require.Equal(t, "default", p["permission_mode"])
			require.Equal(t, "PreToolUse", p["hook_event_name"])
			require.Equal(t, "bash", p["tool_name"])
			require.Equal(t, "id9", p["tool_use_id"])
			require.NotContains(t, p, "tool_response")
			ti, ok := p["tool_input"].(map[string]any)
			require.True(t, ok, "tool_input must be an object")
			if name == "object" {
				require.Equal(t, "ls", ti["command"])
			} else {
				require.Empty(t, ti)
			}
		})
	}
}

func TestPreChainRewritesAndDenyStops(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	marker := filepath.Join(dir, "marker")
	first := script(t, `echo '{"hookSpecificOutput":{"updatedInput":{"command":"one"}}}'`)
	second := script(t, fmt.Sprintf(`cat > %q; echo '{"hookSpecificOutput":{"updatedInput":{"command":"two"}}}'`, seen))
	deny := script(t, `echo "stop" >&2; exit 2`)
	never := script(t, fmt.Sprintf(`touch %q`, marker))
	r := newRunner(load(t, PreToolUse, "", handlerJSON(t, first, nil), handlerJSON(t, second, nil)))

	d := gate(t, r, nil, bashCall)
	require.Equal(t, durable.ActionAllow, d.Action)
	require.JSONEq(t, `{"command":"two"}`, string(d.Input))
	b, err := os.ReadFile(seen)
	require.NoError(t, err)
	require.Contains(t, string(b), `"command":"one"`)

	r = newRunner(load(t, PreToolUse, "", handlerJSON(t, first, nil), handlerJSON(t, deny, nil), handlerJSON(t, never, nil)))
	d = gate(t, r, nil, bashCall)
	require.Equal(t, durable.ActionDeny, d.Action)
	require.Equal(t, "stop", d.Reason)
	require.NoFileExists(t, marker)
}

func TestPrePrecedence(t *testing.T) {
	decide := func(d string) string {
		return handlerJSON(t, script(t, `echo '{"hookSpecificOutput":{"permissionDecision":"`+d+`"}}'`), nil)
	}
	for name, tc := range map[string]struct {
		hooks  []string
		action durable.Action
	}{
		"allow then ask":  {[]string{decide("allow"), decide("ask")}, durable.ActionAsk},
		"ask then allow":  {[]string{decide("ask"), decide("allow")}, durable.ActionAsk},
		"ask then deny":   {[]string{decide("ask"), decide("deny")}, durable.ActionDeny},
		"deny then allow": {[]string{decide("deny"), decide("allow")}, durable.ActionDeny},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRunner(load(t, PreToolUse, "", tc.hooks...))
			baseCalled := false
			base := func(context.Context, durable.ToolCall) (durable.Decision, error) {
				baseCalled = true
				return durable.Decision{Action: durable.ActionAllow}, nil
			}
			require.Equal(t, tc.action, gate(t, r, base, bashCall).Action)
			require.False(t, baseCalled, "a hook decision skips base")
		})
	}
}

func TestPreBase(t *testing.T) {
	rewriteHook := script(t, `echo '{"hookSpecificOutput":{"updatedInput":{"command":"hooked"}}}'`)
	r := newRunner(load(t, PreToolUse, "", handlerJSON(t, rewriteHook, nil)))

	var got durable.ToolCall
	ask := func(_ context.Context, c durable.ToolCall) (durable.Decision, error) {
		got = c
		return durable.Decision{Action: durable.ActionAsk}, nil
	}
	d := gate(t, r, ask, bashCall)
	require.Equal(t, durable.ActionAsk, d.Action, "base action is kept")
	require.JSONEq(t, `{"command":"hooked"}`, string(got.Input), "base sees the rewritten call")
	require.JSONEq(t, `{"command":"hooked"}`, string(d.Input), "hook rewrite is returned")

	baseRewrites := func(context.Context, durable.ToolCall) (durable.Decision, error) {
		return durable.Decision{Action: durable.ActionAllow, Input: json.RawMessage(`{"command":"base"}`)}, nil
	}
	d = gate(t, r, baseRewrites, bashCall)
	require.JSONEq(t, `{"command":"base"}`, string(d.Input), "base rewrite wins")

	baseErr := errors.New("boom")
	_, err := r.Gate(func(context.Context, durable.ToolCall) (durable.Decision, error) {
		return durable.Decision{}, baseErr
	})(context.Background(), bashCall)
	require.ErrorIs(t, err, baseErr)

	d = gate(t, r, nil, bashCall)
	require.Equal(t, durable.ActionAllow, d.Action, "nil base allows")
	require.JSONEq(t, `{"command":"hooked"}`, string(d.Input))

	noop := preRunner(t, `exit 0`)
	d = gate(t, noop, ask, bashCall)
	require.Equal(t, durable.ActionAsk, d.Action)
	require.Nil(t, d.Input)
}

func TestPreOnFailure(t *testing.T) {
	failing := script(t, `exit 1`)
	allow := script(t, `echo '{"hookSpecificOutput":{"permissionDecision":"allow"}}'`)

	r := newRunner(load(t, PreToolUse, "", handlerJSON(t, failing, map[string]any{"onFailure": "continue"}), handlerJSON(t, allow, nil)))
	require.Equal(t, durable.ActionAllow, gate(t, r, nil, bashCall).Action)

	r = newRunner(load(t, PreToolUse, "", handlerJSON(t, failing, map[string]any{"onFailure": "block"}), handlerJSON(t, allow, nil)))
	require.Equal(t, durable.ActionDeny, gate(t, r, nil, bashCall).Action)

	badOutput := script(t, `echo '{bad'`)
	r = newRunner(load(t, PreToolUse, "", handlerJSON(t, badOutput, map[string]any{"onFailure": "continue"})))
	require.Equal(t, durable.ActionAllow, gate(t, r, nil, bashCall).Action)
}

func TestPreStartFailureDenies(t *testing.T) {
	logDir := t.TempDir()
	r := New(load(t, PreToolUse, "", handlerJSON(t, filepath.Join(t.TempDir(), "absent"), map[string]any{"args": []string{"x"}})), Options{LogDir: logDir})
	d := gate(t, r, nil, bashCall)
	requireBlocked(t, d, logDir, "failed to start")
}

func TestExecFormRunsWithoutShell(t *testing.T) {
	out := filepath.Join(t.TempDir(), "args")
	sh := script(t, fmt.Sprintf(`printf '%%s|' "$@" > %q`, out))
	r := newRunner(load(t, PreToolUse, "", handlerJSON(t, sh, map[string]any{"args": []string{"a b", "$HOME"}})))
	gate(t, r, nil, bashCall)
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, "a b|$HOME|", string(b))
}

func TestEmptyArgsRunsWithoutShell(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "evaluated")
	// With a shell this would run the substitution; as an executable name it can't start.
	for _, command := range []string{
		"echo hi $(touch " + marker + ")",
		"touch " + marker,
	} {
		r := New(load(t, PreToolUse, "", handlerJSON(t, command, map[string]any{"args": []string{}})), Options{LogDir: dir})
		d := gate(t, r, nil, bashCall)
		requireBlocked(t, d, dir, "failed to start")
		require.NoFileExists(t, marker)
	}

	// A plain path with empty args still runs as an executable.
	ran := filepath.Join(dir, "ran")
	r := newRunner(load(t, PreToolUse, "", handlerJSON(t, script(t, "touch "+ran), map[string]any{"args": []string{}})))
	gate(t, r, nil, bashCall)
	require.FileExists(t, ran)
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	logDir := t.TempDir()
	r := New(load(t, PreToolUse, "", handlerJSON(t, script(t, fmt.Sprintf(`sleep 60 & echo $! > %q; sleep 60`, pidFile)), map[string]any{"timeout": 1})), Options{LogDir: logDir})

	start := time.Now()
	d, err := r.Gate(nil)(context.Background(), bashCall)
	require.NoError(t, err, "a hook timeout is a deny, not an error")
	requireBlocked(t, d, logDir, "timed out")
	require.Less(t, time.Since(start), 10*time.Second)

	b, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}, 5*time.Second, 20*time.Millisecond, "background child must be killed with the group")
}

func TestBackgroundChildHoldingStdoutDoesNotHang(t *testing.T) {
	r := preRunner(t, `sleep 60 &
echo '{"hookSpecificOutput":{"permissionDecision":"allow"}}'`)
	start := time.Now()
	d := gate(t, r, nil, bashCall)
	require.Equal(t, durable.ActionAllow, d.Action)
	require.Less(t, time.Since(start), 15*time.Second)
}

func TestOversizedStdoutFails(t *testing.T) {
	r, logDir := loggedRunner(t, `head -c 2000000 /dev/zero | tr '\0' 'a'; sleep 60`)
	start := time.Now()
	d := gate(t, r, nil, bashCall)
	requireBlocked(t, d, logDir, "stdout")
	require.Less(t, time.Since(start), 15*time.Second, "overflow kills the hook")
}

func TestStdoutAtLimitIsAccepted(t *testing.T) {
	r := preRunner(t, `head -c 1048576 /dev/zero | tr '\0' 'a'`)
	require.Equal(t, durable.ActionAllow, gate(t, r, nil, bashCall).Action)
}

func TestHookThatIgnoresStdinDoesNotDeadlock(t *testing.T) {
	big, err := json.Marshal(map[string]string{"command": strings.Repeat("x", 8<<20)})
	require.NoError(t, err)
	r := preRunner(t, `sleep 1; exit 0`)

	done := make(chan durable.Decision, 1)
	go func() {
		d, _ := r.Gate(nil)(context.Background(), durable.ToolCall{ID: "c", Name: "bash", Input: big})
		done <- d
	}()
	select {
	case d := <-done:
		require.Equal(t, durable.ActionAllow, d.Action)
	case <-time.After(20 * time.Second):
		t.Fatal("hook deadlocked on stdin")
	}
}

func TestParentContextEndedReturnsError(t *testing.T) {
	r := preRunner(t, `sleep 60`)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	_, err := r.Gate(nil)(ctx, bashCall)
	require.ErrorIs(t, err, context.Canceled)

	ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = r.Gate(nil)(ctx, bashCall)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestEnvIsScrubbed(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-anthropic")
	t.Setenv("OPENROUTER_API_KEY", "sk-openrouter")
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "op-token")
	t.Setenv("DURABLE_TEST_VISIBLE", "visible")
	out := filepath.Join(t.TempDir(), "env")
	r := preRunner(t, fmt.Sprintf(`env > %q`, out))
	gate(t, r, nil, bashCall)

	b, err := os.ReadFile(out)
	require.NoError(t, err)
	env := string(b)
	require.NotContains(t, env, "ANTHROPIC_")
	require.NotContains(t, env, "OPENROUTER_")
	require.NotContains(t, env, "OP_SERVICE")
	require.Contains(t, env, "DURABLE_TEST_VISIBLE=visible")
}

func TestTempCwdIsRemoved(t *testing.T) {
	workspace := t.TempDir()
	out := filepath.Join(t.TempDir(), "cwd")
	r := New(load(t, PreToolUse, "", handlerJSON(t, script(t, fmt.Sprintf(`pwd -P > %q; echo $PWD >> %q; touch here`, out, out)), nil)),
		Options{Workspace: workspace})
	gate(t, r, nil, bashCall)

	b, err := os.ReadFile(out)
	require.NoError(t, err)
	lines := strings.Fields(string(b))
	require.Len(t, lines, 2)
	for _, l := range lines {
		require.NotEqual(t, workspace, l)
		require.NoDirExists(t, l)
	}
	require.NoFileExists(t, filepath.Join(workspace, "here"))
}

func TestUseWorkspaceCwd(t *testing.T) {
	workspace := t.TempDir()
	r := New(load(t, PreToolUse, "", handlerJSON(t, script(t, `touch here`), map[string]any{"useWorkspaceCwd": true})),
		Options{Workspace: workspace})
	gate(t, r, nil, bashCall)
	require.FileExists(t, filepath.Join(workspace, "here"))
}

func TestFailuresAreLogged(t *testing.T) {
	logDir := t.TempDir()
	cmd := script(t, `echo "diagnostic detail" >&2; exit 1`)
	r := New(load(t, PreToolUse, "", handlerJSON(t, cmd, nil)), Options{LogDir: logDir})
	d := gate(t, r, nil, bashCall)
	require.NotContains(t, d.Reason, "diagnostic detail", "stderr stays out of the model's text")

	path := filepath.Join(logDir, "hooks.log")
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	log := string(b)
	require.Contains(t, log, "event=PreToolUse")
	require.Contains(t, log, "tool=bash")
	require.Contains(t, log, cmd)
	require.Contains(t, log, "diagnostic detail")

	gate(t, r, nil, bashCall)
	b, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(string(b), "event=PreToolUse"), "entries are appended")
}

func TestExitTwoIsNotLogged(t *testing.T) {
	logDir := t.TempDir()
	r := New(load(t, PreToolUse, "", handlerJSON(t, script(t, `exit 2`), nil)), Options{LogDir: logDir})
	gate(t, r, nil, bashCall)
	require.NoFileExists(t, filepath.Join(logDir, "hooks.log"))
}

func TestLogFailureDoesNotFailHook(t *testing.T) {
	for _, dir := range []string{"", filepath.Join(t.TempDir(), "absent", "dir")} {
		r := New(load(t, PreToolUse, "", handlerJSON(t, script(t, `exit 1`), nil)), Options{LogDir: dir})
		d, err := r.Gate(nil)(context.Background(), bashCall)
		require.NoError(t, err)
		require.Equal(t, durable.ActionDeny, d.Action)
	}
}

func TestExitTwoStderrIsBounded(t *testing.T) {
	r := preRunner(t, `head -c 500000 /dev/zero | tr '\0' 'e' >&2; exit 2`)
	d := gate(t, r, nil, bashCall)
	require.Equal(t, durable.ActionDeny, d.Action)
	require.Len(t, d.Reason, maxStderr)
}

// fakeTool returns a fixed result and implements the optional interfaces.
type fakeTool struct {
	name     string
	out      string
	err      error
	readOnly bool
	recover  string
}

func (f fakeTool) Spec() durable.ToolSpec                           { return durable.ToolSpec{Name: f.name} }
func (f fakeTool) Replay() durable.Replay                           { return durable.ReplaySafe }
func (f fakeTool) ReadOnly() bool                                   { return f.readOnly }
func (f fakeTool) Recover(context.Context, durable.ToolCall) string { return f.recover }
func (f fakeTool) Execute(context.Context, durable.ToolCall) (string, error) {
	return f.out, f.err
}

func postRun(t *testing.T, r *Runner, tool fakeTool) (string, error) {
	t.Helper()
	wrapped := r.Wrap([]durable.Tool{tool})
	return wrapped[0].Execute(context.Background(), durable.ToolCall{ID: "c1", Name: tool.name, Input: json.RawMessage(`{"command":"ls"}`)})
}

func postRunner(t *testing.T, body string, extra map[string]any) *Runner {
	t.Helper()
	return newRunner(load(t, PostToolUse, "bash", handlerJSON(t, script(t, body), extra)))
}

func TestPostOutputForms(t *testing.T) {
	hso := func(s string) string { return `echo '{"hookSpecificOutput":{` + s + `}}'` }
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"empty stdout":     {`exit 0`, "original"},
		"non json stdout":  {`echo hi`, "original"},
		"string":           {hso(`"updatedToolOutput":"redacted"`), "redacted"},
		"empty string":     {hso(`"updatedToolOutput":""`), ""},
		"object":           {hso(`"updatedToolOutput":{"output":"redacted"}`), "redacted"},
		"additional":       {hso(`"additionalContext":"note"`), "original\nnote"},
		"update and note":  {hso(`"updatedToolOutput":"r","additionalContext":"note"`), "r\nnote"},
		"block":            {`echo '{"decision":"block","reason":"careful"}'`, "original\ncareful"},
		"exit 2 appends":   {`echo "heads up" >&2; exit 2`, "original\nheads up"},
		"exit 2 no stderr": {`exit 2`, "original"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := postRun(t, postRunner(t, tc.body, nil), fakeTool{name: "bash", out: "original"})
			require.NoError(t, err)
			require.Equal(t, tc.want, out)
		})
	}
}

func TestPostFailureWithholdsOutput(t *testing.T) {
	for name, body := range map[string]string{
		"exit 1":           `echo secret >&2; exit 1`,
		"bad json":         `echo '{bad'`,
		"number":           `echo '{"hookSpecificOutput":{"updatedToolOutput":5}}'`,
		"null":             `echo '{"hookSpecificOutput":{"updatedToolOutput":null}}'`,
		"object no output": `echo '{"hookSpecificOutput":{"updatedToolOutput":{"text":"x"}}}'`,
		"object extra key": `echo '{"hookSpecificOutput":{"updatedToolOutput":{"output":"x","y":1}}}'`,
		"unknown decision": `echo '{"decision":"approve"}'`,
	} {
		t.Run(name, func(t *testing.T) {
			out, err := postRun(t, postRunner(t, body, nil), fakeTool{name: "bash", out: "SECRET-ORIGINAL"})
			require.EqualError(t, err, Withheld)
			require.Empty(t, out)
		})
	}
}

func TestPostOnFailureContinueKeepsOriginal(t *testing.T) {
	out, err := postRun(t, postRunner(t, `exit 1`, map[string]any{"onFailure": "continue"}), fakeTool{name: "bash", out: "original"})
	require.NoError(t, err)
	require.Equal(t, "original", out)
}

func TestPostTimeoutWithholds(t *testing.T) {
	_, err := postRun(t, postRunner(t, `sleep 60`, map[string]any{"timeout": 1}), fakeTool{name: "bash", out: "original"})
	require.EqualError(t, err, Withheld)
}

func TestPostErrorResultsReachHookAndStayErrors(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "stdin.json")
	r := postRunner(t, fmt.Sprintf(`cat > %q; echo '{"hookSpecificOutput":{"updatedToolOutput":"redacted"}}'`, capture), nil)

	out, err := postRun(t, r, fakeTool{name: "bash", err: errors.New("command exited with code 1; output: token=abc")})
	require.EqualError(t, err, "redacted")
	require.Empty(t, out)

	b, rerr := os.ReadFile(capture)
	require.NoError(t, rerr)
	var p struct {
		Event    string `json:"hook_event_name"`
		Response struct {
			Output  string `json:"output"`
			IsError bool   `json:"is_error"`
		} `json:"tool_response"`
	}
	require.NoError(t, json.Unmarshal(b, &p))
	require.Equal(t, "PostToolUse", p.Event)
	require.True(t, p.Response.IsError)
	require.Contains(t, p.Response.Output, "token=abc")

	out, err = postRun(t, r, fakeTool{name: "bash", out: "fine"})
	require.NoError(t, err)
	require.Equal(t, "redacted", out)
	b, rerr = os.ReadFile(capture)
	require.NoError(t, rerr)
	require.Contains(t, string(b), `"is_error":false`)
}

func TestPostUnchangedErrorKeepsText(t *testing.T) {
	_, err := postRun(t, postRunner(t, `exit 0`, nil), fakeTool{name: "bash", err: errors.New("boom")})
	require.EqualError(t, err, "boom")
}

func TestPostChainSeesPreviousOutput(t *testing.T) {
	first := script(t, `echo '{"hookSpecificOutput":{"updatedToolOutput":"one"}}'`)
	seen := filepath.Join(t.TempDir(), "seen")
	second := script(t, fmt.Sprintf(`cat > %q; echo '{"hookSpecificOutput":{"updatedToolOutput":"two"}}'`, seen))
	r := newRunner(load(t, PostToolUse, "", handlerJSON(t, first, nil), handlerJSON(t, second, nil)))
	out, err := postRun(t, r, fakeTool{name: "bash", out: "original"})
	require.NoError(t, err)
	require.Equal(t, "two", out)
	b, rerr := os.ReadFile(seen)
	require.NoError(t, rerr)
	require.Contains(t, string(b), `"output":"one"`)
}

func TestPostSkipsNonMatchingTool(t *testing.T) {
	r := postRunner(t, `exit 1`, nil)
	out, err := postRun(t, r, fakeTool{name: "grep", out: "original"})
	require.NoError(t, err)
	require.Equal(t, "original", out)
}

func TestPostParentContextEndedReturnsError(t *testing.T) {
	r := postRunner(t, `sleep 60`, nil)
	wrapped := r.Wrap([]durable.Tool{fakeTool{name: "bash", out: "original"}})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	_, err := wrapped[0].Execute(ctx, bashCall)
	require.ErrorIs(t, err, context.Canceled)
}

func TestWrapForwardsOptionalInterfaces(t *testing.T) {
	r := newRunner(Config{})
	wrapped := r.Wrap([]durable.Tool{
		fakeTool{name: "a", readOnly: true, recover: "note"},
		bare{fakeTool{name: "b"}},
	})

	rec, ok := wrapped[0].(durable.Recoverer)
	require.True(t, ok)
	require.Equal(t, "note", rec.Recover(context.Background(), bashCall))
	ro, ok := wrapped[0].(durable.ReadOnlyTool)
	require.True(t, ok)
	require.True(t, ro.ReadOnly())
	require.Equal(t, "a", wrapped[0].Spec().Name)
	require.Equal(t, durable.ReplaySafe, wrapped[0].Replay())

	rec, ok = wrapped[1].(durable.Recoverer)
	require.True(t, ok)
	require.Empty(t, rec.Recover(context.Background(), bashCall))
	ro, ok = wrapped[1].(durable.ReadOnlyTool)
	require.True(t, ok)
	require.False(t, ro.ReadOnly())

	require.Empty(t, r.Wrap(nil))
}

// bare hides the optional interfaces of the tool it wraps.
type bare struct{ f fakeTool }

func (b bare) Spec() durable.ToolSpec { return b.f.Spec() }
func (b bare) Replay() durable.Replay { return b.f.Replay() }
func (b bare) Execute(ctx context.Context, c durable.ToolCall) (string, error) {
	return b.f.Execute(ctx, c)
}
