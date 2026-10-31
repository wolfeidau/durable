package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/wolfeidau/durable"
)

type fakeModel struct {
	mu    sync.Mutex
	calls int
	fn    func(n int) (durable.Message, error)
}

func (m *fakeModel) Generate(context.Context, []durable.Message, []durable.ToolSpec) (durable.Message, error) {
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.mu.Unlock()
	return m.fn(n)
}

func (m *fakeModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

type fakeTool struct {
	name   string
	replay durable.Replay
	mu     sync.Mutex
	runs   int
}

func (f *fakeTool) Spec() durable.ToolSpec { return durable.ToolSpec{Name: f.name} }
func (f *fakeTool) Replay() durable.Replay { return f.replay }
func (f *fakeTool) Execute(context.Context, durable.ToolCall) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs++
	return "ok", nil
}

func (f *fakeTool) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs
}

func toolCallMsg(name string) durable.Message {
	return durable.Message{Role: durable.RoleAssistant, ToolCalls: []durable.ToolCall{
		{ID: "c1", Name: name, Input: json.RawMessage(`{"command":"echo hi"}`)},
	}}
}

func textMsg(s string) durable.Message {
	return durable.Message{Role: durable.RoleAssistant, Text: s}
}

// run drives a repl over input to EOF and returns the output.
func run(t *testing.T, model durable.Model, tool durable.Tool, yes bool, input string) (*durable.Harness, string, error) {
	t.Helper()
	lines := readLines(strings.NewReader(input))
	out := &bytes.Buffer{}
	opts := durable.Options{
		Model:         model,
		Tools:         withDisplay([]durable.Tool{tool}, out),
		LeaseTTL:      300 * time.Millisecond,
		ModelAttempts: 1,
		ModelBackoff:  time.Millisecond,
	}
	if !yes {
		opts.Approve = newApprover(lines, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	h, err := durable.Open(ctx, filepath.Join(t.TempDir(), "s.sqlite"), opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	r := &repl{h: h, lines: lines, sigs: make(chan os.Signal), out: out}
	err = r.run(ctx)
	return h, out.String(), err
}

func roles(t *testing.T, h *durable.Harness) []durable.Role {
	t.Helper()
	hist, err := h.History(context.Background())
	require.NoError(t, err)
	var rs []durable.Role
	for _, m := range hist {
		rs = append(rs, m.Role)
	}
	return rs
}

func toolThenText(n int) (durable.Message, error) {
	if n == 1 {
		return toolCallMsg("bash"), nil
	}
	return textMsg("done"), nil
}

func TestApprovalYes(t *testing.T) {
	tool := &fakeTool{name: "bash"}
	h, out, err := run(t, &fakeModel{fn: toolThenText}, tool, false, "go\ny\n")
	require.NoError(t, err)
	require.Equal(t, 1, tool.count())
	require.Contains(t, out, "echo hi")
	require.Equal(t, []durable.Role{durable.RoleUser, durable.RoleAssistant, durable.RoleTool, durable.RoleAssistant}, roles(t, h))
}

func TestApprovalNo(t *testing.T) {
	tool := &fakeTool{name: "bash"}
	h, _, err := run(t, &fakeModel{fn: toolThenText}, tool, false, "go\nmaybe\n")
	require.NoError(t, err)
	require.Zero(t, tool.count())
	hist, err := h.History(context.Background())
	require.NoError(t, err)
	require.True(t, hist[2].IsError)
	require.Equal(t, durable.Declined, hist[2].Text)
}

func TestApprovalEOFLeavesCallPending(t *testing.T) {
	tool := &fakeTool{name: "bash"}
	h, out, err := run(t, &fakeModel{fn: toolThenText}, tool, false, "go\n")
	require.NoError(t, err)
	require.Zero(t, tool.count())
	require.Contains(t, out, "stdin closed")
	require.Equal(t, []durable.Role{durable.RoleUser, durable.RoleAssistant}, roles(t, h))
}

func TestYesSkipsApproval(t *testing.T) {
	tool := &fakeTool{name: "bash"}
	_, out, err := run(t, &fakeModel{fn: toolThenText}, tool, true, "go\n")
	require.NoError(t, err)
	require.Equal(t, 1, tool.count())
	require.NotContains(t, out, "[y/N]")
}

func TestReadOnlyToolsNeverAsk(t *testing.T) {
	approve := newApprover(make(chan string), &bytes.Buffer{})
	for _, name := range []string{"read_file", "list_files", "grep"} {
		ok, err := approve(context.Background(), durable.ToolCall{Name: name, Input: json.RawMessage(`{}`)})
		require.NoError(t, err)
		require.True(t, ok)
	}
}

func TestEditApprovalShowsSummary(t *testing.T) {
	lines := make(chan string, 1)
	lines <- "YES"
	out := &bytes.Buffer{}
	ok, err := newApprover(lines, out)(context.Background(), durable.ToolCall{
		Name: "edit_file", Input: json.RawMessage(`{"path":"a.go","old_string":"foo","new_string":"bar"}`),
	})
	require.NoError(t, err)
	require.True(t, ok)
	require.Contains(t, out.String(), "a.go")
	require.Contains(t, out.String(), "old: foo")
	require.Contains(t, out.String(), "new: bar")
}

func TestApprovalCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ok, err := newApprover(make(chan string), &bytes.Buffer{})(ctx, durable.ToolCall{Name: "bash", Input: json.RawMessage(`{}`)})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, ok)
}

func TestBlankLinesSkipped(t *testing.T) {
	m := &fakeModel{fn: func(int) (durable.Message, error) { return textMsg("hi"), nil }}
	_, _, err := run(t, m, &fakeTool{name: "bash"}, true, "\n   \nhello\n\n")
	require.NoError(t, err)
	require.Equal(t, 1, m.count())
}

func TestFailedSubmissionKeepsLooping(t *testing.T) {
	m := &fakeModel{fn: func(n int) (durable.Message, error) {
		if n == 1 {
			return durable.Message{}, errors.New("bad request")
		}
		return textMsg("fine"), nil
	}}
	h, out, err := run(t, m, &fakeTool{name: "bash"}, true, "one\ntwo\n")
	require.NoError(t, err)
	require.Contains(t, out, "failed: ")
	require.Equal(t, 2, m.count())
	hist, err := h.History(context.Background())
	require.NoError(t, err)
	require.Equal(t, "fine", hist[len(hist)-1].Text)
}

func TestTransientPauseResumesOnNextInput(t *testing.T) {
	m := &fakeModel{fn: func(n int) (durable.Message, error) {
		if n == 1 {
			return durable.Message{}, durable.Transient(errors.New("overloaded"))
		}
		return textMsg("reply"), nil
	}}
	h, out, err := run(t, m, &fakeTool{name: "bash"}, true, "first\nsecond\n")
	require.NoError(t, err)
	require.Contains(t, out, "overloaded")
	hist, err := h.History(context.Background())
	require.NoError(t, err)
	var got []string
	for _, msg := range hist {
		got = append(got, string(msg.Role)+":"+msg.Text)
	}
	require.Equal(t, []string{"user:first", "assistant:reply", "user:second", "assistant:reply"}, got)
}

func TestSessionPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a, err := SessionPath("/work/a", "default")
	require.NoError(t, err)
	again, err := SessionPath("/work/a", "default")
	require.NoError(t, err)
	require.Equal(t, a, again)

	otherSession, err := SessionPath("/work/a", "other")
	require.NoError(t, err)
	otherDir, err := SessionPath("/work/b", "default")
	require.NoError(t, err)
	require.NotEqual(t, a, otherSession)
	require.NotEqual(t, a, otherDir)

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, ".durable", "sessions"), filepath.Dir(filepath.Dir(a)))
	require.Len(t, filepath.Base(filepath.Dir(a)), 16)
	require.Equal(t, "session.sqlite", filepath.Base(a))
}

func TestDisplayToolPreservesOrderSpecAndReplay(t *testing.T) {
	inner := []durable.Tool{
		&fakeTool{name: "read_file", replay: durable.ReplaySafe},
		&fakeTool{name: "bash", replay: durable.ReplayNever},
	}
	out := &bytes.Buffer{}
	wrapped := withDisplay(inner, out)
	require.Len(t, wrapped, 2)
	for i := range inner {
		require.Equal(t, inner[i].Spec(), wrapped[i].Spec())
		require.Equal(t, inner[i].Replay(), wrapped[i].Replay())
	}

	res, err := wrapped[1].Execute(context.Background(), durable.ToolCall{Name: "bash", Input: json.RawMessage(`{"command":"ls -la"}`)})
	require.NoError(t, err)
	require.Equal(t, "ok", res)
	require.Contains(t, out.String(), "[bash] ls -la")
}
