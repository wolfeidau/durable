package commands

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/tools"
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
	return runREPL(t, model, tool, yes, false, input)
}

func runREPL(t *testing.T, model durable.Model, tool durable.Tool, yes, piped bool, input string) (*durable.Harness, string, error) {
	t.Helper()
	lines := readLines(strings.NewReader(input))
	out := &bytes.Buffer{}
	opts := durable.Options{
		Model:         model,
		Tools:         withDisplay([]durable.Tool{tool}, out, nil),
		LeaseTTL:      300 * time.Millisecond,
		ModelAttempts: 1,
		ModelBackoff:  time.Millisecond,
	}
	if !yes {
		opts.Ask = newApprover(lines, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	h, err := durable.Open(ctx, filepath.Join(t.TempDir(), "s.sqlite"), opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	r := &repl{h: h, lines: lines, sigs: make(chan os.Signal), out: out, piped: piped}
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
	toolset, err := tools.All(t.TempDir(), t.TempDir())
	require.NoError(t, err)
	readOnly := readOnlyTools(toolset)
	require.Equal(t, map[string]bool{"read_file": true, "list_files": true, "grep": true}, readOnly)

	gate := durable.ReadOnlyGate(toolset)
	for name := range readOnly {
		d, err := gate(context.Background(), durable.ToolCall{Name: name, Input: json.RawMessage(`{}`)})
		require.NoError(t, err)
		require.Equal(t, durable.ActionAllow, d.Action)
	}
}

func TestUndeclaredToolsAsk(t *testing.T) {
	replaySafe := &fakeTool{name: "fetch", replay: durable.ReplaySafe}
	d, err := durable.ReadOnlyGate([]durable.Tool{replaySafe})(context.Background(), durable.ToolCall{Name: "fetch"})
	require.NoError(t, err)
	require.Equal(t, durable.ActionAsk, d.Action)

	for answer, want := range map[string]bool{"y": true, "n": false} {
		lines := make(chan string, 1)
		lines <- answer
		out := &bytes.Buffer{}
		ok, err := newApprover(lines, out)(context.Background(), durable.ToolCall{
			Name: "fetch", Input: json.RawMessage(`{"url":"https://example.com"}`),
		})
		require.NoError(t, err)
		require.Equal(t, want, ok)
		require.Contains(t, out.String(), "[fetch]")
		require.Contains(t, out.String(), "https://example.com")
		require.Contains(t, out.String(), "[y/N]")
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

func TestResumedFailureIsReported(t *testing.T) {
	ctx := context.Background()
	m := &fakeModel{fn: func(int) (durable.Message, error) { return durable.Message{}, errors.New("bad request") }}
	h, err := durable.Open(ctx, filepath.Join(t.TempDir(), "session.sqlite"), durable.Options{Model: m})
	require.NoError(t, err)
	defer h.Close()
	_, err = h.Submit(ctx, "left-over", "from a previous process")
	require.NoError(t, err)

	out := &bytes.Buffer{}
	r := &repl{h: h, lines: readLines(strings.NewReader("")), out: out}
	require.NoError(t, r.run(ctx))
	require.Contains(t, out.String(), "failed: model failed: bad request")
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

func TestPipedExitStatus(t *testing.T) {
	bad := func(fail int) func(int) (durable.Message, error) {
		return func(n int) (durable.Message, error) {
			if n == fail {
				return durable.Message{}, errors.New("bad request")
			}
			return textMsg("fine"), nil
		}
	}
	overloadedOnce := func(n int) (durable.Message, error) {
		if n == 1 {
			return durable.Message{}, durable.Transient(errors.New("overloaded"))
		}
		return textMsg("fine"), nil
	}
	tests := []struct {
		name    string
		fn      func(int) (durable.Message, error)
		yes     bool
		input   string
		wantErr string
	}{
		{name: "success", fn: bad(0), yes: true, input: "one\n"},
		{name: "failed submission", fn: bad(1), yes: true, input: "one\n", wantErr: "submissions failed"},
		{name: "failure outlives a later success", fn: bad(1), yes: true, input: "one\ntwo\n", wantErr: "submissions failed"},
		{name: "stdin closed at approval", fn: toolThenText, input: "go\n", wantErr: "stdin closed"},
		{name: "pause left unfinished", fn: overloadedOnce, yes: true, input: "one\n", wantErr: "overloaded"},
		{name: "pause finished by next turn", fn: overloadedOnce, yes: true, input: "one\ntwo\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := runREPL(t, &fakeModel{fn: tt.fn}, &fakeTool{name: "bash"}, tt.yes, true, tt.input)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestResumedFailureFailsPipedExit(t *testing.T) {
	ctx := context.Background()
	m := &fakeModel{fn: func(int) (durable.Message, error) { return durable.Message{}, errors.New("bad request") }}
	h, err := durable.Open(ctx, filepath.Join(t.TempDir(), "session.sqlite"), durable.Options{Model: m})
	require.NoError(t, err)
	defer h.Close()
	_, err = h.Submit(ctx, "left-over", "from a previous process")
	require.NoError(t, err)

	r := &repl{h: h, lines: readLines(strings.NewReader("")), out: &bytes.Buffer{}, piped: true}
	require.ErrorContains(t, r.run(ctx), "submissions failed")
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ctxTool blocks until its context is cancelled.
type ctxTool struct {
	name    string
	entered chan struct{}
	mu      sync.Mutex
	runs    int
}

func (c *ctxTool) Spec() durable.ToolSpec { return durable.ToolSpec{Name: c.name} }
func (c *ctxTool) Replay() durable.Replay { return durable.ReplayNever }
func (c *ctxTool) Execute(ctx context.Context, _ durable.ToolCall) (string, error) {
	c.mu.Lock()
	c.runs++
	c.mu.Unlock()
	c.entered <- struct{}{}
	<-ctx.Done()
	return "", ctx.Err()
}

func (c *ctxTool) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs
}

// sigRepl is a repl driven interactively: the test types lines and sends signals.
type sigRepl struct {
	h     *durable.Harness
	stdin *io.PipeWriter
	path  string
	sigs  chan os.Signal
	out   *lockedBuffer
	done  chan error
}

func startSigRepl(t *testing.T, model durable.Model, tool durable.Tool, yes bool) *sigRepl {
	t.Helper()
	pr, pw := io.Pipe()
	lines := readLines(pr)
	out := &lockedBuffer{}
	opts := durable.Options{
		Model:         model,
		Tools:         withDisplay([]durable.Tool{tool}, out, nil),
		LeaseTTL:      300 * time.Millisecond,
		ModelAttempts: 1,
		ModelBackoff:  time.Millisecond,
	}
	if !yes {
		opts.Ask = newApprover(lines, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	path := filepath.Join(t.TempDir(), "s.sqlite")
	h, err := durable.Open(ctx, path, opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	s := &sigRepl{h: h, path: path, stdin: pw, sigs: make(chan os.Signal, 2), out: out, done: make(chan error, 1)}
	r := &repl{h: h, lines: lines, sigs: s.sigs, out: out}
	go func() { s.done <- r.run(ctx) }()
	t.Cleanup(func() { _ = pw.Close() })
	return s
}

func (s *sigRepl) typeLine(t *testing.T, line string) {
	t.Helper()
	_, err := io.WriteString(s.stdin, line+"\n")
	require.NoError(t, err)
}

func (s *sigRepl) waitOutput(t *testing.T, want string) {
	t.Helper()
	require.Eventually(t, func() bool { return strings.Contains(s.out.String(), want) }, 10*time.Second, 5*time.Millisecond, "output %q", want)
}

func (s *sigRepl) finish(t *testing.T) {
	t.Helper()
	require.NoError(t, s.stdin.Close())
	select {
	case err := <-s.done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("repl did not exit")
	}
}

func (s *sigRepl) requireNothingUnfinished(t *testing.T) {
	t.Helper()
	left, err := s.h.Unfinished(context.Background())
	require.NoError(t, err)
	require.Empty(t, left)
}

func TestSIGINTDuringToolCancelsAndDoesNotResume(t *testing.T) {
	tool := &ctxTool{name: "bash", entered: make(chan struct{}, 2)}
	m := &fakeModel{fn: toolThenText}
	s := startSigRepl(t, m, tool, true)

	s.typeLine(t, "go")
	<-tool.entered
	s.sigs <- syscall.SIGINT
	s.waitOutput(t, "cancelled")
	s.requireNothingUnfinished(t)

	s.typeLine(t, "next")
	s.waitOutput(t, "> ")
	s.finish(t)
	require.Equal(t, 1, tool.count())
	require.Equal(t, 2, m.count())
	hist, err := s.h.History(context.Background())
	require.NoError(t, err)
	require.Equal(t, durable.CancelledUnknown, hist[2].Text)
	require.True(t, hist[2].IsError)
	require.Equal(t, durable.RequestCancelled, hist[3].Text)
}

func TestSIGINTAtApprovalCancelsAndToolNeverRuns(t *testing.T) {
	tool := &ctxTool{name: "bash", entered: make(chan struct{}, 2)}
	m := &fakeModel{fn: toolThenText}
	s := startSigRepl(t, m, tool, false)

	s.typeLine(t, "go")
	s.waitOutput(t, "[y/N]")
	s.sigs <- syscall.SIGINT
	s.waitOutput(t, "cancelled")
	s.requireNothingUnfinished(t)

	s.typeLine(t, "next")
	s.finish(t)
	require.Zero(t, tool.count())
	require.Equal(t, 2, m.count())
	hist, err := s.h.History(context.Background())
	require.NoError(t, err)
	require.Equal(t, durable.CallCancelled, hist[2].Text)
	require.Equal(t, durable.RequestCancelled, hist[3].Text)
}

func TestSIGTERMDuringTurnKeepsSubmissionRunning(t *testing.T) {
	tool := &ctxTool{name: "bash", entered: make(chan struct{}, 2)}
	s := startSigRepl(t, &fakeModel{fn: toolThenText}, tool, true)

	s.typeLine(t, "go")
	<-tool.entered
	s.sigs <- syscall.SIGTERM
	select {
	case err := <-s.done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("repl did not exit")
	}
	require.NotContains(t, s.out.String(), "cancelled")
	left, err := s.h.Unfinished(context.Background())
	require.NoError(t, err)
	require.Len(t, left, 1)
	require.Equal(t, durable.StatusRunning, left[0].Status)
}

func TestSIGINTCancelsPausedSubmissionAndNewPrompt(t *testing.T) {
	entered := make(chan struct{}, 2)
	m := &fakeModel{fn: func(n int) (durable.Message, error) {
		if n == 1 {
			return durable.Message{}, durable.Transient(errors.New("overloaded"))
		}
		return textMsg("unused"), nil
	}}
	blocking := blockOnCtx{inner: m, entered: entered}
	s := startSigRepl(t, blocking, &fakeTool{name: "bash"}, true)

	s.typeLine(t, "first")
	s.waitOutput(t, "overloaded")
	s.typeLine(t, "second")
	<-entered
	s.sigs <- syscall.SIGINT
	s.waitOutput(t, "cancelled")
	s.requireNothingUnfinished(t)

	s.finish(t)
	hist, err := s.h.History(context.Background())
	require.NoError(t, err)
	require.Equal(t, []durable.Role{durable.RoleUser, durable.RoleUser}, roles(t, s.h))
	require.Len(t, hist, 2)
	require.Equal(t, durable.RequestCancelled, hist[1].Text)
}

// blockOnCtx delegates the first model call, then blocks later ones until cancelled.
type blockOnCtx struct {
	inner   *fakeModel
	entered chan struct{}
}

func (b blockOnCtx) Generate(ctx context.Context, h []durable.Message, ts []durable.ToolSpec) (durable.Message, error) {
	if b.inner.count() == 0 {
		return b.inner.Generate(ctx, h, ts)
	}
	b.entered <- struct{}{}
	<-ctx.Done()
	return durable.Message{}, ctx.Err()
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
	wrapped := withDisplay(inner, out, nil)
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

type recoverTool struct{ *fakeTool }

func (recoverTool) Recover(_ context.Context, call durable.ToolCall) string {
	return "recovered " + call.ID
}

func TestDisplayToolForwardsRecover(t *testing.T) {
	wrapped := withDisplay([]durable.Tool{recoverTool{&fakeTool{name: "bash"}}, &fakeTool{name: "read_file"}}, &bytes.Buffer{}, nil)
	r, ok := wrapped[0].(durable.Recoverer)
	require.True(t, ok)
	require.Equal(t, "recovered c1", r.Recover(context.Background(), durable.ToolCall{ID: "c1"}))

	r, ok = wrapped[1].(durable.Recoverer)
	require.True(t, ok)
	require.Empty(t, r.Recover(context.Background(), durable.ToolCall{ID: "c1"}))
}

// stuckTool ignores cancellation until released, like a tool slow to stop.
type stuckTool struct {
	started chan struct{}
	release chan struct{}
}

func (s *stuckTool) Spec() durable.ToolSpec { return durable.ToolSpec{Name: "bash"} }
func (s *stuckTool) Replay() durable.Replay { return durable.ReplayNever }
func (s *stuckTool) Execute(context.Context, durable.ToolCall) (string, error) {
	close(s.started)
	<-s.release
	return "", nil
}

func TestSecondInterruptKeepsLease(t *testing.T) {
	tool := &stuckTool{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(tool.release) })
	s := startSigRepl(t, &fakeModel{fn: toolThenText}, tool, true)
	s.typeLine(t, "go")
	<-tool.started
	s.sigs <- syscall.SIGINT
	s.sigs <- syscall.SIGINT
	require.ErrorContains(t, <-s.done, "interrupted twice")
	require.NoError(t, s.h.Close())

	db, err := sql.Open("sqlite3", s.path)
	require.NoError(t, err)
	defer db.Close()
	var leases int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM lease`).Scan(&leases))
	require.Equal(t, 1, leases)
}

func TestReadLinesLongLineAndFinalLine(t *testing.T) {
	long := strings.Repeat("x", 5<<20)
	var got []string
	for l := range readLines(strings.NewReader(long + "\r\nsecond\nlast")) {
		got = append(got, l)
	}
	require.Equal(t, []string{long, "second", "last"}, got)
}

func TestDisplayToolQuietSkipsHeader(t *testing.T) {
	out := &bytes.Buffer{}
	wrapped := withDisplay([]durable.Tool{&fakeTool{name: "bash"}, &fakeTool{name: "read_file"}}, out, map[string]bool{"bash": true})
	_, err := wrapped[0].Execute(context.Background(), durable.ToolCall{Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)})
	require.NoError(t, err)
	require.Empty(t, out.String())
	_, err = wrapped[1].Execute(context.Background(), durable.ToolCall{Name: "read_file", Input: json.RawMessage(`{"path":"a"}`)})
	require.NoError(t, err)
	require.Contains(t, out.String(), "[read_file] a")
}

func TestTruncateKeepsUTF8(t *testing.T) {
	got := truncate("aé", 2)
	require.Equal(t, "a...", got)
	require.True(t, utf8.ValidString(got))
}
