package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/commands"
	"github.com/wolfeidau/durable/internal/fakeapi"
)

const (
	routerKey       = "or-test-key"
	anthropicSecret = "anthropic-test-secret"
	routerModel     = "openai/test-model"
)

// routerAPI plays OpenRouter: a bash call on the first request of either protocol, then text.
type routerAPI struct {
	srv     *httptest.Server
	command string

	mu      sync.Mutex
	paths   []string
	bodies  []string
	headers []http.Header
}

func newRouterAPI(t *testing.T, command string) *routerAPI {
	t.Helper()
	f := &routerAPI{command: command}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.bodies = append(f.bodies, string(b))
		f.headers = append(f.headers, r.Header.Clone())
		first := len(f.bodies) == 1
		f.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		switch r.URL.Path {
		case "/v1/responses":
			item := fakeapi.ResponsesMessageItem("msg_1", "all done")
			if first {
				args, _ := json.Marshal(map[string]any{"command": f.command, "timeout_seconds": nil})
				item = fakeapi.ResponsesCallItem("fc_1", "call_1", "bash", string(args))
			}
			_, _ = io.WriteString(w, fakeapi.ResponsesItem(0, item)+fakeapi.ResponsesCompleted("["+item+"]"))
		case "/v1/messages":
			if first {
				_, _ = io.WriteString(w, bashReply(f.command))
				return
			}
			_, _ = io.WriteString(w, textReply("all done"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *routerAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *routerAPI) snapshot() (paths, bodies []string, headers []http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...), append([]string(nil), f.bodies...), append([]http.Header(nil), f.headers...)
}

// routerEnv gives the binary an OpenRouter key and URL plus Anthropic values that must never be used.
func routerEnv(home, routerURL, anthropicURL string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "OPENROUTER_") && !strings.HasPrefix(kv, "ANTHROPIC_") {
			env = append(env, kv)
		}
	}
	env = append(env, "HOME="+home, "OPENROUTER_BASE_URL="+routerURL, "OPENROUTER_API_KEY="+routerKey,
		"ANTHROPIC_BASE_URL="+anthropicURL, "ANTHROPIC_API_KEY="+anthropicSecret, "ANTHROPIC_AUTH_TOKEN="+anthropicSecret)
	return env
}

func startRouterAgent(t *testing.T, bin, ws string, env []string, args ...string) *agent {
	t.Helper()
	cmd := exec.Command(bin, append(args, "--lease-ttl", "500ms")...)
	cmd.Dir = ws
	cmd.Env = env
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return &agent{cmd: cmd, stdin: stdin, out: out}
}

func closeAndWait(t *testing.T, a *agent) {
	t.Helper()
	require.NoError(t, a.stdin.Close())
	done := make(chan error, 1)
	go func() { done <- a.cmd.Wait() }()
	select {
	case err := <-done:
		require.NoError(t, err, a.out.String())
	case <-time.After(testTimeout):
		t.Fatalf("agent did not exit: %s", a.out.String())
	}
}

// runRouterPrompt sends one prompt, waits for the tool call and its follow-up, then exits cleanly.
func runRouterPrompt(t *testing.T, api *routerAPI, a *agent) {
	t.Helper()
	_, err := io.WriteString(a.stdin, "go\n")
	require.NoError(t, err)
	eventually(t, "API requests", func() bool { return api.count() == 2 })
	closeAndWait(t, a)
}

// failingStart runs the binary with stdin closed and expects a startup error.
func failingStart(t *testing.T, bin, ws string, env []string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = ws
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	require.Error(t, err, string(out))
	return string(out)
}

// unusedAnthropic fails the test if the Anthropic URL is ever contacted.
func unusedAnthropic(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to the Anthropic URL: %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func sessionDir(t *testing.T, ws string) string {
	t.Helper()
	path, err := commands.SessionPath(ws, "default")
	require.NoError(t, err)
	return filepath.Dir(path)
}

func pinFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "*.json"))
	require.NoError(t, err)
	return m
}

func TestOpenRouterDefaultProtocolUsesResponses(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	api := newRouterAPI(t, "echo ran >> marker.txt")
	env := routerEnv(home, api.srv.URL, unusedAnthropic(t))

	a := startRouterAgent(t, bin, ws, env, "--yes", "--provider", "openrouter", "--model", routerModel)
	runRouterPrompt(t, api, a)

	paths, bodies, headers := api.snapshot()
	require.Equal(t, []string{"/v1/responses", "/v1/responses"}, paths)
	require.Equal(t, "Bearer "+routerKey, headers[0].Get("Authorization"))
	require.Equal(t, filepath.Base(sessionDir(t, ws)), headers[0].Get("x-session-id"))
	require.Contains(t, bodies[0], `"model":"`+routerModel+`"`)
	require.Contains(t, a.out.String(), "[model] openrouter via openai-responses: "+routerModel)
	require.NotContains(t, a.out.String(), "warning")
	require.Len(t, readLinesOf(t, filepath.Join(ws, "marker.txt")), 1)
}

func TestOpenRouterMessagesProtocolUsesGatewayMode(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	api := newRouterAPI(t, "true")
	env := routerEnv(home, api.srv.URL, unusedAnthropic(t))

	a := startRouterAgent(t, bin, ws, env, "--yes", "--provider", "openrouter", "--protocol", "anthropic-messages", "--model", "anthropic/claude-sonnet-5-5")
	runRouterPrompt(t, api, a)

	paths, bodies, headers := api.snapshot()
	require.Equal(t, []string{"/v1/messages", "/v1/messages"}, paths)
	for i, h := range headers {
		require.Equal(t, "Bearer "+routerKey, h.Get("Authorization"))
		require.Empty(t, h.Values("x-api-key"))
		require.Empty(t, h.Values("anthropic-beta"))
		require.NotContains(t, strings.Join(h.Values("Authorization"), ","), anthropicSecret)
		require.NotContains(t, bodies[i], "fallbacks")
		require.NotContains(t, bodies[i], "context_management")
	}
	require.Contains(t, a.out.String(), "[model] openrouter via anthropic-messages: anthropic/claude-sonnet-5-5")
}

func TestKillDuringBashResumesOnEachProtocol(t *testing.T) {
	bin := buildBinary(t)
	for _, tc := range []struct {
		name string
		args []string
		path string
	}{
		{"responses", []string{"--provider", "openrouter", "--model", routerModel}, "/v1/responses"},
		{"messages", []string{"--provider", "openrouter", "--protocol", "anthropic-messages", "--model", "anthropic/claude-sonnet-5-5"}, "/v1/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, home := newWorkspace(t)
			marker := filepath.Join(ws, "marker.txt")
			api := newRouterAPI(t, "echo ran >> marker.txt; sleep 5")
			env := routerEnv(home, api.srv.URL, unusedAnthropic(t))

			a := startRouterAgent(t, bin, ws, env, append([]string{"--yes"}, tc.args...)...)
			_, err := io.WriteString(a.stdin, "run it\n")
			require.NoError(t, err)
			eventually(t, "marker line", func() bool { return len(readLinesOf(t, marker)) == 1 })
			a.kill(t)

			// The provider, protocol and model come from the pin.
			b := startRouterAgent(t, bin, ws, env, "--yes")
			closeAndWait(t, b)
			require.Contains(t, b.out.String(), "[model] openrouter via ")

			require.Len(t, readLinesOf(t, marker), 1, "bash must not run twice")
			paths, bodies, _ := api.snapshot()
			require.Equal(t, []string{tc.path, tc.path}, paths)
			require.Contains(t, bodies[1], durable.UnknownOutcome)
			if tc.path == "/v1/responses" {
				require.Contains(t, bodies[1], `"function_call_output"`)
				// Only replayed Raw keeps the item id.
				require.Contains(t, bodies[1], `"fc_1"`)
			} else {
				require.True(t, hasToolResult(t, bodies[1], true, durable.UnknownOutcome), bodies[1])
			}
		})
	}
}

func TestOpenRouterRouteCannotChangeOnResume(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	api := newRouterAPI(t, "true")
	env := routerEnv(home, api.srv.URL, unusedAnthropic(t))
	a := startRouterAgent(t, bin, ws, env, "--yes", "--provider", "openrouter", "--model", routerModel)
	runRouterPrompt(t, api, a)

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--provider", "anthropic"}, "uses provider openrouter, not anthropic"},
		{[]string{"--protocol", "anthropic-messages"}, "uses protocol openai-responses, not anthropic-messages"},
		{[]string{"--model", "openai/other"}, "uses model " + routerModel + ", not openai/other"},
	} {
		require.Contains(t, failingStart(t, bin, ws, env, tc.args...), tc.want)
	}
	require.Equal(t, 2, api.count())
}

func TestProviderRejectsUnsupportedProtocol(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	env := routerEnv(home, "http://127.0.0.1:1", unusedAnthropic(t))

	out := failingStart(t, bin, ws, env, "--provider", "anthropic", "--protocol", "openai-responses")
	require.Contains(t, out, "provider anthropic does not support protocol")
	require.Contains(t, out, "anthropic-messages")
	require.Empty(t, pinFiles(t, sessionDir(t, ws)))
}

func TestAnthropicModelOnResponsesWarns(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	api := newRouterAPI(t, "true")
	env := routerEnv(home, api.srv.URL, unusedAnthropic(t))

	a := startRouterAgent(t, bin, ws, env, "--yes", "--provider", "openrouter", "--model", "anthropic/claude-sonnet-5-5")
	runRouterPrompt(t, api, a)
	require.Contains(t, a.out.String(), "[model] openrouter via openai-responses: anthropic/claude-sonnet-5-5")
	require.Contains(t, a.out.String(), "[model] warning:")
	require.Contains(t, a.out.String(), "--protocol anthropic-messages")
}

func TestFailedOpenRouterStartLeavesNoPin(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	api := newRouterAPI(t, "true")
	env := routerEnv(home, api.srv.URL, unusedAnthropic(t))
	dir := sessionDir(t, ws)
	require.NoError(t, os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("marker DRAFT"), 0o644))

	out := failingStart(t, bin, ws, env, "--provider", "openrouter")
	require.Contains(t, out, "--model is required")

	var noKey []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "OPENROUTER_API_KEY=") {
			noKey = append(noKey, kv)
		}
	}
	out = failingStart(t, bin, ws, noKey, "--provider", "openrouter", "--model", routerModel)
	require.Contains(t, out, "OPENROUTER_API_KEY")

	require.Empty(t, pinFiles(t, dir))
	_, err := os.Stat(filepath.Join(dir, "instructions.md"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(dir, "session.sqlite"))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Zero(t, api.count())
}

func TestResumedOpenRouterSessionStillNeedsKey(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	api := newRouterAPI(t, "true")
	env := routerEnv(home, api.srv.URL, unusedAnthropic(t))
	a := startRouterAgent(t, bin, ws, env, "--yes", "--provider", "openrouter", "--model", routerModel)
	runRouterPrompt(t, api, a)

	var noKey []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "OPENROUTER_API_KEY=") {
			noKey = append(noKey, kv)
		}
	}
	require.Contains(t, failingStart(t, bin, ws, noKey), "OPENROUTER_API_KEY")
}

func TestCorruptProviderPinIsAStartupError(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	env := routerEnv(home, "http://127.0.0.1:1", unusedAnthropic(t))
	dir := sessionDir(t, ws)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "provider.json"), []byte("{not json"), 0o600))

	require.Contains(t, failingStart(t, bin, ws, env), "provider")
}

func TestSessionWithoutProviderPinStaysAnthropic(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	api := newFakeAPIReply(t, textReply("hi"))

	a := startAgent(t, bin, ws, home, api.srv.URL)
	runPrompt(t, api, 1, a, "first")
	require.Contains(t, a.out.String(), "[model] anthropic via anthropic-messages: claude-sonnet-5-5")

	pin := filepath.Join(sessionDir(t, ws), "provider.json")
	require.NoError(t, os.Remove(pin))
	b := startAgent(t, bin, ws, home, api.srv.URL)
	runPrompt(t, api, 2, b, "second")
	require.Contains(t, b.out.String(), "[model] anthropic via anthropic-messages")
	pinned, err := os.ReadFile(pin)
	require.NoError(t, err)
	require.JSONEq(t, `{"provider":"anthropic","protocol":"anthropic-messages"}`, string(pinned))

	require.Contains(t, failingStart(t, bin, ws, routerEnv(home, "http://127.0.0.1:1", api.srv.URL), "--provider", "openrouter", "--model", routerModel),
		"uses provider anthropic, not openrouter")
}

func TestBashDoesNotSeeOpenRouterKey(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	api := newRouterAPI(t, "env > env.txt")
	env := routerEnv(home, api.srv.URL, unusedAnthropic(t))

	a := startRouterAgent(t, bin, ws, env, "--yes", "--provider", "openrouter", "--model", routerModel)
	runRouterPrompt(t, api, a)

	got, err := os.ReadFile(filepath.Join(ws, "env.txt"))
	require.NoError(t, err)
	require.NotEmpty(t, got)
	require.NotContains(t, string(got), "OPENROUTER_")
	require.NotContains(t, string(got), routerKey)
}
