package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
)

const (
	liveTimeout = 4 * time.Minute
	liveWord    = "kumquat"
)

// liveEnv drops Anthropic credentials and fake URLs, so only the real OpenRouter route can work.
func liveEnv(home string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "ANTHROPIC_") && !strings.HasPrefix(kv, "OPENROUTER_BASE_URL=") {
			env = append(env, kv)
		}
	}
	return append(env, "HOME="+home)
}

// waitLive stops at the first failed submission, so a bad key or model doesn't wait out the timeout.
func waitLive(t *testing.T, a *agent, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(liveTimeout)
	for !cond() {
		out := a.out.String()
		if strings.Contains(out, "failed: model failed:") || time.Now().After(deadline) {
			t.Fatalf("waiting for %s: %s", what, out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A crash mid-tool then a second turn replays stored Raw twice against the real service.
func TestOpenRouterLive(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" || os.Getenv("DURABLE_SMOKE") != "1" {
		t.Skip("set OPENROUTER_API_KEY and DURABLE_SMOKE=1 to run against the real API")
	}
	bin := buildBinary(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"responses-reasoning", []string{"--model", "openai/gpt-6-luna", "--effort", "low"}},
		{"responses-non-openai", []string{"--model", "z-ai/glm-5.3-flash"}},
		{"messages-claude", []string{"--protocol", "anthropic-messages", "--model", "anthropic/claude-haiku-5.5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, home := newWorkspace(t)
			require.NoError(t, os.WriteFile(filepath.Join(ws, "note.txt"), []byte(liveWord+"\n"), 0o600))
			env := liveEnv(home)

			a := startRouterAgent(t, bin, ws, env, append([]string{"--yes", "--provider", "openrouter"}, tc.args...)...)
			_, err := io.WriteString(a.stdin, "First use bash to run exactly this command: `echo ran >> marker.txt; sleep 30`. Then read note.txt with read_file and reply with only the word in it.\n")
			require.NoError(t, err)
			waitLive(t, a, "bash to run", func() bool { return len(readLinesOf(t, filepath.Join(ws, "marker.txt"))) > 0 })
			a.kill(t)

			b := startRouterAgent(t, bin, ws, env, "--yes")
			_, err = io.WriteString(b.stdin, "Now reply with only that word from note.txt in uppercase.\n")
			require.NoError(t, err)
			// Only the model writes the uppercase form, so tool output can't satisfy this.
			waitLive(t, b, "the uppercase reply", func() bool { return strings.Contains(b.out.String(), strings.ToUpper(liveWord)) })
			closeAndWait(t, b)
			require.NotContains(t, a.out.String()+b.out.String(), key)

			var unknown bool
			var transcript strings.Builder
			for _, m := range finalHistory(t, ws) {
				unknown = unknown || (m.Role == durable.RoleTool && strings.Contains(m.Text, durable.UnknownOutcome))
				fmt.Fprintf(&transcript, "%s calls=%v text=%q\n", m.Role, m.ToolCalls, m.Text)
			}
			require.True(t, unknown, "the killed bash call must be recorded as an unknown outcome:\n%s", transcript.String())
		})
	}
}
