package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/fakeapi"
)

const apiModel = "claude-opus-5-5"

func bashReply(command string) string {
	input, _ := json.Marshal(map[string]any{"command": command, "timeout_seconds": nil})
	return fakeapi.Stream(apiModel, "tool_use", "null", fakeapi.ToolBlock(0, "toolu_1", "bash", string(input)))
}

func textReply(text string) string {
	return fakeapi.Stream(apiModel, "end_turn", "null", fakeapi.TextBlock(0, text))
}

// fakeAPI answers the first request with firstReply and every later one with end_turn text, each after delay.
type fakeAPI struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newFakeAPI(t *testing.T, firstReply string, delay time.Duration) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(b))
		first := len(f.bodies) == 1
		f.mu.Unlock()

		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
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

// useAPI points every process the test starts at the fake API.
func useAPI(t *testing.T, api *fakeAPI) {
	t.Helper()
	t.Setenv("ANTHROPIC_BASE_URL", api.srv.URL)
	t.Setenv("ANTHROPIC_API_KEY", "dummy")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
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

// chatOnce runs durable chat with one prompt on stdin, which exits at EOF once the prompt is done.
func chatOnce(t *testing.T, ws, prompt string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, durablePath, append([]string{"chat", "--session", "s"}, args...)...)
	cmd.Dir = ws
	cmd.Stdin = strings.NewReader(prompt)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

// sessionDB returns the single session's ID and database path under home.
func sessionDB(t *testing.T, home string) (id, dbPath string) {
	t.Helper()
	metas, err := filepath.Glob(filepath.Join(home, ".durable", "sessions", "*", "session.json"))
	require.NoError(t, err)
	require.Len(t, metas, 1)
	dir := filepath.Dir(metas[0])
	return filepath.Base(dir), filepath.Join(dir, "session.sqlite")
}

type idleModel struct{}

func (idleModel) Generate(context.Context, []durable.Message, []durable.ToolSpec) (durable.Message, error) {
	return durable.Message{}, errors.New("model must not be called")
}

// openSession opens the session directly, which works only while no server holds it.
func openSession(t *testing.T, dbPath string) *durable.Harness {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	h, err := durable.Open(ctx, dbPath, durable.Options{Model: idleModel{}})
	require.NoError(t, err)
	return h
}

// queue commits submissions the server will run once it opens the session.
func queue(t *testing.T, dbPath string, texts ...string) {
	t.Helper()
	h := openSession(t, dbPath)
	defer h.Close()
	for i, text := range texts {
		_, err := h.Submit(t.Context(), "req-"+string(rune('a'+i)), text)
		require.NoError(t, err)
	}
}

// allEvents reads the whole log, so the session must not be held by a server.
func allEvents(t *testing.T, dbPath string) []durable.Event {
	t.Helper()
	h := openSession(t, dbPath)
	defer h.Close()
	events, err := h.Events(t.Context(), 0, 0)
	require.NoError(t, err)
	return events
}
