package main

import (
	"bytes"
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

	"github.com/wolfeidau/durable/internal/commands"
	"github.com/wolfeidau/durable/internal/fakeapi"
)

const (
	testTimeout = 60 * time.Second
	apiModel    = "claude-opus-5-5"
)

func bashReply(command string) string {
	input, _ := json.Marshal(map[string]any{"command": command, "timeout_seconds": nil})
	return fakeapi.Stream(apiModel, "tool_use", "null", fakeapi.ToolBlock(0, "toolu_1", "bash", string(input)))
}

func textReply(text string) string {
	return fakeapi.Stream(apiModel, "end_turn", "null", fakeapi.TextBlock(0, text))
}

// fakeAPI answers the first request with a canned reply and every later one with end_turn text.
type fakeAPI struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newFakeAPI(t *testing.T, firstReply string) *fakeAPI {
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
			_, _ = io.WriteString(w, firstReply)
			return
		}
		_, _ = io.WriteString(w, textReply("all done"))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

// apiEnv points the Anthropic client at the fake API.
func apiEnv(api *fakeAPI) []string {
	return []string{"ANTHROPIC_BASE_URL=" + api.srv.URL, "ANTHROPIC_API_KEY=dummy", "ANTHROPIC_AUTH_TOKEN="}
}

func newWorkspace(t *testing.T) (workspace, home string) {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	hm, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return ws, hm
}

// createSession has the durable binary create a session; an empty stdin exits without calling the model.
func createSession(t *testing.T, ws, home, name string) string {
	t.Helper()
	chat := exec.CommandContext(t.Context(), durablePath, "chat", "--session", name)
	chat.Dir = ws
	chat.Env = append(os.Environ(), "HOME="+home, "ANTHROPIC_API_KEY=dummy", "ANTHROPIC_AUTH_TOKEN=")
	out, err := chat.CombinedOutput()
	require.NoError(t, err, string(out))
	metas, _, err := commands.ListSessions(home)
	require.NoError(t, err)
	for _, m := range metas {
		if m.Name == name && m.CWD == ws {
			return m.ID
		}
	}
	t.Fatalf("session %q not created", name)
	return ""
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

type agent struct {
	stdin io.WriteCloser
	out   *syncBuffer
}

// startChat runs durable chat with stdin held open, so it keeps the session lease.
func startChat(t *testing.T, ws, home, name string, api *fakeAPI) *agent {
	t.Helper()
	cmd := exec.Command(durablePath, "chat", "--session", name, "--lease-ttl", "500ms")
	cmd.Dir = ws
	cmd.Env = append(append(os.Environ(), "HOME="+home), apiEnv(api)...)
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return &agent{stdin: stdin, out: out}
}

// newHoldingAPI holds its first response until the client disconnects, so a request stays in flight.
func newHoldingAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, "")
		first := len(f.bodies) == 1
		f.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if first {
			select {
			case <-r.Context().Done():
			case <-time.After(testTimeout):
			}
			return
		}
		_, _ = io.WriteString(w, textReply("all done"))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	return strings.Fields(string(b))
}
