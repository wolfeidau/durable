package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/commands"
	"github.com/wolfeidau/durable/internal/fakeapi"
	"github.com/wolfeidau/durable/tools"
)

const testTimeout = 90 * time.Second

const apiModel = "claude-opus-5-5"

func bashReply(command string) string {
	input, _ := json.Marshal(map[string]any{"command": command, "timeout_seconds": nil})
	return fakeapi.Stream(apiModel, "tool_use", "null", fakeapi.ToolBlock(0, "toolu_1", "bash", string(input)))
}

func textReply(text string) string {
	return fakeapi.Stream(apiModel, "end_turn", "null", fakeapi.TextBlock(0, text))
}

// fakeAPI answers the first request with a bash tool call and every later one with end_turn text.
type fakeAPI struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newFakeAPI(t *testing.T, command string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(b))
		first := len(f.bodies) == 1
		f.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if first {
			_, _ = io.WriteString(w, bashReply(command))
			return
		}
		_, _ = io.WriteString(w, textReply("all done"))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "durable")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", "build", "-o", bin, ".").CombinedOutput()
	require.NoError(t, err, string(out))
	return bin
}

type agent struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *syncBuffer
}

func startAgent(t *testing.T, bin, workspace, home, apiURL string, args ...string) *agent {
	t.Helper()
	args = append(args, "--lease-ttl", "500ms")
	cmd := exec.Command(bin, args...)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "HOME="+home, "ANTHROPIC_BASE_URL="+apiURL, "ANTHROPIC_API_KEY=dummy", "ANTHROPIC_AUTH_TOKEN=")
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return &agent{cmd: cmd, stdin: stdin, out: out}
}

func (a *agent) kill(t *testing.T) {
	t.Helper()
	require.NoError(t, a.cmd.Process.Signal(syscall.SIGKILL))
	_ = a.cmd.Wait() // reports "signal: killed"
	_ = a.stdin.Close()
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	require.Eventually(t, cond, testTimeout, 20*time.Millisecond, "timed out waiting for %s", what)
}

func readLinesOf(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	return strings.Fields(string(b))
}

// restart runs the agent again with stdin closed and expects a clean exit.
func restart(t *testing.T, bin, workspace, home, apiURL string, args ...string) {
	t.Helper()
	a := startAgent(t, bin, workspace, home, apiURL, args...)
	require.NoError(t, a.stdin.Close())
	done := make(chan error, 1)
	go func() { done <- a.cmd.Wait() }()
	select {
	case err := <-done:
		require.NoError(t, err, a.out.String())
	case <-time.After(testTimeout):
		t.Fatalf("restarted agent did not exit: %s", a.out.String())
	}
}

type failModel struct{ t *testing.T }

func (m failModel) Generate(context.Context, []durable.Message, []durable.ToolSpec) (durable.Message, error) {
	m.t.Error("model called, but nothing should be left to do")
	return durable.Message{}, fmt.Errorf("unexpected model call")
}

// finalHistory checks the session is fully resumed: Run has nothing to do and returns the transcript.
func finalHistory(t *testing.T, workspace string) []durable.Message {
	t.Helper()
	path, err := commands.SessionPath(workspace, "default")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	h, err := durable.Open(ctx, path, durable.Options{Model: failModel{t}, LeaseTTL: 500 * time.Millisecond})
	require.NoError(t, err)
	defer h.Close()
	require.NoError(t, h.Run(ctx))
	hist, err := h.History(ctx)
	require.NoError(t, err)
	return hist
}

func historyRoles(hist []durable.Message) []durable.Role {
	var rs []durable.Role
	for _, m := range hist {
		rs = append(rs, m.Role)
	}
	return rs
}

func hasToolResult(t *testing.T, body string, isError bool, text string) bool {
	t.Helper()
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	for _, m := range req.Messages {
		var blocks []struct {
			Type    string          `json:"type"`
			IsError bool            `json:"is_error"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "tool_result" && b.IsError == isError && strings.Contains(string(b.Content), text) {
				return true
			}
		}
	}
	return false
}

func newWorkspace(t *testing.T) (workspace, home string) {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	hm, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", hm)
	return ws, hm
}

func TestKillDuringToolCallReportsUnknownOutcome(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	marker := filepath.Join(ws, "marker.txt")
	api := newFakeAPI(t, "echo ran >> marker.txt; sleep 5")

	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	_, err := io.WriteString(a.stdin, "run it\n")
	require.NoError(t, err)
	eventually(t, "marker line", func() bool { return len(readLinesOf(t, marker)) == 1 })
	a.kill(t)

	restart(t, bin, ws, home, api.srv.URL, "--yes")

	require.Len(t, readLinesOf(t, marker), 1, "bash must not run twice")
	reqs := api.requests()
	require.Len(t, reqs, 2)
	require.True(t, hasToolResult(t, reqs[1], true, durable.UnknownOutcome), reqs[1])

	hist := finalHistory(t, ws)
	require.Equal(t, []durable.Role{durable.RoleUser, durable.RoleAssistant, durable.RoleTool, durable.RoleAssistant}, historyRoles(hist))
	require.Equal(t, "all done", hist[3].Text)
}

func TestKillDuringApprovalKeepsCallPending(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	marker := filepath.Join(ws, "marker.txt")
	api := newFakeAPI(t, "echo ran >> marker.txt")

	a := startAgent(t, bin, ws, home, api.srv.URL)
	_, err := io.WriteString(a.stdin, "run it\n")
	require.NoError(t, err)
	eventually(t, "approval prompt", func() bool { return strings.Contains(a.out.String(), "[y/N]") })
	require.Empty(t, readLinesOf(t, marker))
	a.kill(t)

	restart(t, bin, ws, home, api.srv.URL, "--yes")

	require.Len(t, readLinesOf(t, marker), 1, "the pending call must run exactly once")
	reqs := api.requests()
	require.Len(t, reqs, 2)
	require.False(t, hasToolResult(t, reqs[1], true, durable.UnknownOutcome), reqs[1])
	require.True(t, hasToolResult(t, reqs[1], false, ""), reqs[1])

	hist := finalHistory(t, ws)
	require.Equal(t, []durable.Role{durable.RoleUser, durable.RoleAssistant, durable.RoleTool, durable.RoleAssistant}, historyRoles(hist))
	require.False(t, hist[2].IsError)
}

func TestInterruptCancelsAndDoesNotResume(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	marker := filepath.Join(ws, "marker.txt")
	api := newFakeAPI(t, "echo ran >> marker.txt; sleep 30")

	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	_, err := io.WriteString(a.stdin, "run it\n")
	require.NoError(t, err)
	eventually(t, "marker line", func() bool { return len(readLinesOf(t, marker)) == 1 })
	require.NoError(t, a.cmd.Process.Signal(syscall.SIGINT))
	eventually(t, "cancelled", func() bool { return strings.Contains(a.out.String(), "cancelled") })

	_, err = io.WriteString(a.stdin, "something else\n")
	require.NoError(t, err)
	eventually(t, "second API request", func() bool { return len(api.requests()) == 2 })
	reqs := api.requests()
	require.True(t, hasToolResult(t, reqs[1], true, durable.CancelledUnknown), reqs[1])

	require.NoError(t, a.stdin.Close())
	done := make(chan error, 1)
	go func() { done <- a.cmd.Wait() }()
	select {
	case err := <-done:
		require.NoError(t, err, a.out.String())
	case <-time.After(testTimeout):
		t.Fatalf("agent did not exit: %s", a.out.String())
	}

	restart(t, bin, ws, home, api.srv.URL, "--yes")

	require.Len(t, readLinesOf(t, marker), 1, "bash must not run again")
	require.Len(t, api.requests(), 2, "a cancelled task must not resume")
	finalHistory(t, ws)
}

func TestKillStopsOrphanedCommand(t *testing.T) {
	bin := buildBinary(t)
	ws, home := newWorkspace(t)
	pidFile := filepath.Join(ws, "pid.txt")
	marker := filepath.Join(ws, "marker.txt")
	api := newFakeAPI(t, "echo $$ > pid.txt; sleep 4; echo late >> marker.txt")

	a := startAgent(t, bin, ws, home, api.srv.URL, "--yes")
	_, err := io.WriteString(a.stdin, "run it\n")
	require.NoError(t, err)
	eventually(t, "pid file", func() bool { return len(readLinesOf(t, pidFile)) == 1 })
	pid, err := strconv.Atoi(readLinesOf(t, pidFile)[0])
	require.NoError(t, err)
	a.kill(t)
	killedAt := time.Now()
	require.NoError(t, syscall.Kill(pid, 0), "the command must outlive the agent")

	restart(t, bin, ws, home, api.srv.URL, "--yes")

	require.Eventually(t, func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH }, 5*time.Second, 20*time.Millisecond)
	reqs := api.requests()
	require.Len(t, reqs, 2)
	require.True(t, hasToolResult(t, reqs[1], true, durable.UnknownOutcome), reqs[1])
	require.True(t, hasToolResult(t, reqs[1], true, tools.OrphanStopped), reqs[1])

	// Wait past the sleep so a surviving command would have written its marker.
	time.Sleep(6*time.Second - time.Since(killedAt))
	require.Empty(t, readLinesOf(t, marker), "the orphan must not finish its command")
}
