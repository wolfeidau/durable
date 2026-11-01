package durable

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type compactorFunc func(ctx context.Context, req CompactRequest) (Compaction, bool, error)

func (f compactorFunc) Compact(ctx context.Context, req CompactRequest) (Compaction, bool, error) {
	return f(ctx, req)
}

// recorder keeps what the fakes were sent.
type recorder struct {
	mu       sync.Mutex
	requests [][]Message
	compacts []CompactRequest
}

func (r *recorder) model(reply func(n int, history []Message) (Message, error)) Model {
	return modelFunc(func(_ context.Context, history []Message, _ []ToolSpec) (Message, error) {
		r.mu.Lock()
		r.requests = append(r.requests, append([]Message(nil), history...))
		n := len(r.requests)
		r.mu.Unlock()
		return reply(n, history)
	})
}

func (r *recorder) compactor(f func(n int, req CompactRequest) (Compaction, bool, error)) Compactor {
	return compactorFunc(func(_ context.Context, req CompactRequest) (Compaction, bool, error) {
		r.mu.Lock()
		r.compacts = append(r.compacts, req)
		n := len(r.compacts)
		r.mu.Unlock()
		return f(n, req)
	})
}

func okReply(_ int, _ []Message) (Message, error) {
	return Message{Role: RoleAssistant, Text: "ok", ContextTokens: 100}, nil
}

func compactionRows(t *testing.T, h *Harness) int {
	t.Helper()
	var n int
	require.NoError(t, h.db.QueryRow(`SELECT COUNT(*) FROM compactions`).Scan(&n))
	return n
}

func eventsOf(t *testing.T, h *Harness, kind EventKind) []Event {
	t.Helper()
	all, err := h.Events(context.Background(), 0, 0)
	require.NoError(t, err)
	var out []Event
	for _, e := range all {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// cutAtLast keeps only the newest message.
func cutAtLast(summary string) func(int, CompactRequest) (Compaction, bool, error) {
	return func(_ int, req CompactRequest) (Compaction, bool, error) {
		if len(req.Messages) < 2 {
			return Compaction{}, false, nil
		}
		return Compaction{Summary: summary, FirstKept: len(req.Messages) - 1}, true, nil
	}
}

func fastOpts(m Model, c Compactor) Options {
	return Options{Model: m, Compactor: c, LeaseTTL: ttl, ModelAttempts: 2, ModelBackoff: time.Millisecond}
}

func TestCompactionSendsSummaryAndKeptMessages(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	h := openWith(t, ctx, fastOpts(rec.model(okReply), rec.compactor(func(n int, _ CompactRequest) (Compaction, bool, error) {
		if n == 2 {
			return Compaction{Summary: "S1", FirstKept: 2}, true, nil
		}
		return Compaction{}, false, nil
	})))
	defer h.Close()

	for _, id := range []string{"a", "b"} {
		_, err := h.Prompt(ctx, id, "msg "+id)
		require.NoError(t, err)
	}

	// Messages: user a, asst, user b (compaction runs before the model sees b).
	last := rec.requests[len(rec.requests)-1]
	require.Len(t, last, 2)
	require.Equal(t, RoleUser, last[0].Role)
	require.Equal(t, CompactionSummaryPrefix+"S1", last[0].Text)
	require.Equal(t, "msg b", last[1].Text)

	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.Equal(t, "user,assistant,user,assistant", roles(hist))
	require.Equal(t, 1, compactionRows(t, h))
	for _, req := range rec.compacts {
		for _, m := range req.Messages {
			require.NotContains(t, m.Text, CompactionSummaryPrefix)
		}
	}
}

func TestCompactionRowAndEventCommitTogether(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	h := openWith(t, ctx, fastOpts(rec.model(okReply), rec.compactor(cutAtLast("secret summary"))))
	defer h.Close()
	for _, id := range []string{"a", "b"} {
		_, err := h.Prompt(ctx, id, "msg "+id)
		require.NoError(t, err)
	}
	rows := compactionRows(t, h)
	evs := eventsOf(t, h, EventCompacted)
	require.Len(t, evs, rows)
	require.Positive(t, rows)
	for _, e := range evs {
		require.NotEqual(t, int64(0), e.Submission)
		require.Empty(t, e.Error)
		require.Nil(t, e.Message)
	}
	var seq, first, through int64
	require.NoError(t, h.db.QueryRow(
		`SELECT submission_seq, first_kept_seq, through_seq FROM compactions ORDER BY seq LIMIT 1`).Scan(&seq, &first, &through))
	require.Equal(t, evs[0].Submission, seq)
	require.Equal(t, through, first) // keeps only the newest message
}

func texts(msgs []Message) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.Text)
	}
	return out
}

func TestSecondCompactionGetsPreviousAndKeptMessages(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	h := openWith(t, ctx, fastOpts(rec.model(okReply), rec.compactor(func(n int, req CompactRequest) (Compaction, bool, error) {
		if len(req.Messages) < 3 {
			return Compaction{}, false, nil
		}
		return Compaction{Summary: "S" + string(rune('0'+n)), FirstKept: len(req.Messages) - 1}, true, nil
	})))
	defer h.Close()
	for _, id := range []string{"a", "b", "c"} {
		_, err := h.Prompt(ctx, id, "msg "+id)
		require.NoError(t, err)
	}
	require.Len(t, rec.compacts, 3)
	require.Empty(t, rec.compacts[1].Previous)
	require.Equal(t, "S2", rec.compacts[2].Previous)
	require.Equal(t, []string{"msg b", "ok", "msg c"}, texts(rec.compacts[2].Messages))
	require.Equal(t, 2, compactionRows(t, h))
}

func TestCompactionZeroesStaleContextTokens(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	h := openWith(t, ctx, fastOpts(rec.model(okReply), rec.compactor(func(_ int, req CompactRequest) (Compaction, bool, error) {
		if len(req.Messages) < 3 {
			return Compaction{}, false, nil
		}
		return Compaction{Summary: "S", FirstKept: len(req.Messages) - 2}, true, nil
	})))
	defer h.Close()
	for _, id := range []string{"a", "b", "c"} {
		_, err := h.Prompt(ctx, id, "msg "+id)
		require.NoError(t, err)
	}
	// Second compaction sees [a1(kept, stale), ub(stale), a2(new), uc].
	req := rec.compacts[2]
	require.Equal(t, []string{"ok", "msg b", "ok", "msg c"}, texts(req.Messages))
	require.Zero(t, req.Messages[0].ContextTokens)
	require.EqualValues(t, 100, req.Messages[2].ContextTokens)

	hist, err := h.History(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 100, hist[1].ContextTokens, "History keeps the stored figure")
}

func TestCompactorSeesNoMessagesWithoutRows(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	h := openWith(t, ctx, fastOpts(rec.model(okReply), rec.compactor(func(int, CompactRequest) (Compaction, bool, error) {
		return Compaction{}, false, nil
	})))
	defer h.Close()
	_, err := h.Prompt(ctx, "a", "hi")
	require.NoError(t, err)
	require.Equal(t, []string{"hi"}, texts(rec.compacts[0].Messages))
	require.Equal(t, []string{"hi"}, texts(rec.requests[0]))
	require.Zero(t, compactionRows(t, h))
}

func TestCompactionCrashDoesNotDuplicateRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sqlite")
	rec := &recorder{}
	crashCtx, cancel := context.WithCancel(context.Background())
	crash := true
	comp := rec.compactor(func(_ int, req CompactRequest) (Compaction, bool, error) {
		if len(req.Messages) < 3 {
			return Compaction{}, false, nil
		}
		if crash {
			crash = false
			cancel()
			return Compaction{}, false, crashCtx.Err()
		}
		return Compaction{Summary: "S", FirstKept: 2}, true, nil
	})
	h1, err := Open(crashCtx, path, fastOpts(rec.model(okReply), comp))
	require.NoError(t, err)
	_, err = h1.Prompt(crashCtx, "a", "msg a")
	require.NoError(t, err)
	_, err = h1.Prompt(crashCtx, "b", "msg b")
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, compactionRows(t, h1))
	h1.abandon()

	ctx := context.Background()
	h2, err := Open(ctx, path, fastOpts(rec.model(okReply), comp))
	require.NoError(t, err)
	defer h2.Close()
	require.NoError(t, h2.Run(ctx))
	sub, err := h2.Get(ctx, "b")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
	require.Equal(t, 1, compactionRows(t, h2))
	require.Len(t, eventsOf(t, h2, EventCompacted), 1)
}

func TestCompactorTransientExhaustedPausesRun(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	cause := errors.New("summariser overloaded")
	down := true
	h := openWith(t, ctx, fastOpts(rec.model(okReply), rec.compactor(func(_ int, req CompactRequest) (Compaction, bool, error) {
		if down {
			return Compaction{}, false, Transient(cause)
		}
		return cutAtLast("S")(0, req)
	})))
	defer h.Close()

	_, err := h.Submit(ctx, "a", "hi")
	require.NoError(t, err)
	require.ErrorIs(t, h.Run(ctx), cause)
	require.Len(t, rec.compacts, 2)
	require.Empty(t, rec.requests, "the model is not called while the compactor is down")
	sub, err := h.Get(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, StatusRunning, sub.Status)
	require.Empty(t, eventsOf(t, h, EventCompactionFailed))

	down = false
	require.NoError(t, h.Run(ctx))
	sub, err = h.Get(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
}

func TestCompactorPermanentErrorIsRecordedAndModelRuns(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	h := openWith(t, ctx, fastOpts(rec.model(okReply), rec.compactor(func(int, CompactRequest) (Compaction, bool, error) {
		return Compaction{}, false, errors.New("bad summary")
	})))
	defer h.Close()
	sub, err := h.Prompt(ctx, "a", "hi")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
	evs := eventsOf(t, h, EventCompactionFailed)
	require.Len(t, evs, 1)
	require.Contains(t, evs[0].Error, "bad summary")
	require.NotZero(t, evs[0].Submission)
	require.Zero(t, compactionRows(t, h))
}

func TestInvalidCompactionIsPermanentFailure(t *testing.T) {
	cases := map[string]Compaction{
		"zero first kept":     {Summary: "S", FirstKept: 0},
		"first kept len":      {Summary: "S", FirstKept: 3},
		"first kept past len": {Summary: "S", FirstKept: 9},
		"negative first kept": {Summary: "S", FirstKept: -1},
		"empty summary":       {Summary: "", FirstKept: 1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			rec := &recorder{}
			h := openWith(t, ctx, fastOpts(rec.model(okReply), rec.compactor(func(_ int, req CompactRequest) (Compaction, bool, error) {
				if len(req.Messages) < 3 {
					return Compaction{}, false, nil
				}
				return c, true, nil
			})))
			defer h.Close()
			for _, id := range []string{"a", "b"} {
				sub, err := h.Prompt(ctx, id, "msg "+id)
				require.NoError(t, err)
				require.Equal(t, StatusDone, sub.Status)
			}
			require.Len(t, eventsOf(t, h, EventCompactionFailed), 1)
			require.Zero(t, compactionRows(t, h))
			require.Len(t, rec.requests, 2)
		})
	}
}

func TestCompactionCutAtToolMessageIsRefused(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	reply := func(n int, _ []Message) (Message, error) {
		if n == 1 {
			return Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "t", Input: json.RawMessage(`{}`)}}}, nil
		}
		return okReply(n, nil)
	}
	opts := fastOpts(rec.model(reply), rec.compactor(func(_ int, req CompactRequest) (Compaction, bool, error) {
		for i, m := range req.Messages {
			if m.Role == RoleTool {
				return Compaction{Summary: "S", FirstKept: i}, true, nil
			}
		}
		return Compaction{}, false, nil
	}))
	opts.Tools = []Tool{&fakeTool{name: "t"}}
	h := openWith(t, ctx, opts)
	defer h.Close()

	sub, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
	require.Len(t, eventsOf(t, h, EventCompactionFailed), 1)
	require.Zero(t, compactionRows(t, h))
}

func TestNilAndEmptyCompactionInputIsHandled(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	h := openWith(t, ctx, fastOpts(rec.model(okReply), rec.compactor(func(int, CompactRequest) (Compaction, bool, error) {
		return Compaction{}, true, nil // ok with an empty compaction
	})))
	defer h.Close()
	sub, err := h.Prompt(ctx, "a", "hi")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
	require.Len(t, eventsOf(t, h, EventCompactionFailed), 1)
}

func overflowModel(overflows int, calls *int) func(int, []Message) (Message, error) {
	return func(n int, _ []Message) (Message, error) {
		*calls = n
		if n <= overflows {
			return Message{}, ContextOverflow(errors.New("prompt is too long"))
		}
		return okReply(n, nil)
	}
}

func TestOverflowForcesOneCompactionAndRetries(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	var calls int
	model := rec.model(func(n int, _ []Message) (Message, error) {
		calls = n
		if n == 2 {
			return Message{}, ContextOverflow(errors.New("prompt is too long"))
		}
		return okReply(n, nil)
	})
	h := openWith(t, ctx, fastOpts(model, rec.compactor(func(_ int, req CompactRequest) (Compaction, bool, error) {
		if !req.Force || len(req.Messages) < 3 {
			return Compaction{}, false, nil
		}
		return Compaction{Summary: "S", FirstKept: 2}, true, nil
	})))
	defer h.Close()
	_, err := h.Prompt(ctx, "a", "msg a")
	require.NoError(t, err)
	sub, err := h.Prompt(ctx, "b", "msg b")
	require.NoError(t, err)
	// a: 1 model call. b: overflow, forced compaction, retry.
	require.Equal(t, StatusDone, sub.Status)
	require.Equal(t, 3, calls)
	var forced int
	for _, c := range rec.compacts {
		if c.Force {
			forced++
		}
	}
	require.Equal(t, 1, forced)
	require.Equal(t, 1, compactionRows(t, h))
	require.Equal(t, CompactionSummaryPrefix+"S", rec.requests[2][0].Text)
}

func TestSecondOverflowFailsSubmission(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	var calls int
	model := rec.model(func(n int, _ []Message) (Message, error) {
		calls = n
		if n == 1 {
			return okReply(n, nil)
		}
		return Message{}, ContextOverflow(errors.New("prompt is too long"))
	})
	h := openWith(t, ctx, fastOpts(model, rec.compactor(func(_ int, req CompactRequest) (Compaction, bool, error) {
		if !req.Force || len(req.Messages) < 3 {
			return Compaction{}, false, nil
		}
		return Compaction{Summary: "S", FirstKept: len(req.Messages) - 1}, true, nil
	})))
	defer h.Close()
	_, err := h.Prompt(ctx, "a", "msg a")
	require.NoError(t, err)
	sub, err := h.Prompt(ctx, "b", "msg b")
	require.NoError(t, err)
	require.Equal(t, StatusFailed, sub.Status)
	require.Contains(t, sub.Error, "model failed: prompt is too long")
	require.Equal(t, 3, calls, "one overflow, one forced compaction, one retry")
	require.Equal(t, 1, compactionRows(t, h))
}

func TestForcedCompactionNotOkFailsSubmission(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	var calls int
	h := openWith(t, ctx, fastOpts(rec.model(overflowModel(9, &calls)), rec.compactor(func(int, CompactRequest) (Compaction, bool, error) {
		return Compaction{}, false, nil
	})))
	defer h.Close()
	sub, err := h.Prompt(ctx, "a", "hi")
	require.NoError(t, err)
	require.Equal(t, StatusFailed, sub.Status)
	require.Contains(t, sub.Error, "model failed: prompt is too long")
	require.Equal(t, 1, calls)
}

func TestForcedCompactionErrorFailsSubmission(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	var calls int
	h := openWith(t, ctx, fastOpts(rec.model(overflowModel(9, &calls)), rec.compactor(func(_ int, req CompactRequest) (Compaction, bool, error) {
		if req.Force {
			return Compaction{}, false, errors.New("summariser broke")
		}
		return Compaction{}, false, nil
	})))
	defer h.Close()
	sub, err := h.Prompt(ctx, "a", "hi")
	require.NoError(t, err)
	require.Equal(t, StatusFailed, sub.Status)
	require.Contains(t, sub.Error, "model failed: prompt is too long")
	require.Len(t, eventsOf(t, h, EventCompactionFailed), 1)
}

func TestOverflowWithoutCompactorFailsAtOnce(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	var calls int
	h := openWith(t, ctx, Options{Model: rec.model(overflowModel(9, &calls)), LeaseTTL: ttl})
	defer h.Close()
	sub, err := h.Prompt(ctx, "a", "hi")
	require.NoError(t, err)
	require.Equal(t, StatusFailed, sub.Status)
	require.Equal(t, "model failed: prompt is too long", sub.Error)
	require.Equal(t, 1, calls)
}

func TestForcedResetsAfterSuccessfulModelStep(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	// Call 1 answers a. For b, calls 2 and 4 overflow and call 3 asks for a tool.
	model := rec.model(func(n int, _ []Message) (Message, error) {
		switch n {
		case 2, 4:
			return Message{}, ContextOverflow(errors.New("context window exceeded"))
		case 3:
			return Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "t", Input: []byte(`{}`)}}}, nil
		}
		return okReply(n, nil)
	})
	opts := fastOpts(model, rec.compactor(func(_ int, req CompactRequest) (Compaction, bool, error) {
		if !req.Force || len(req.Messages) < 2 {
			return Compaction{}, false, nil
		}
		cut := len(req.Messages) - 1
		for req.Messages[cut].Role == RoleTool {
			cut--
		}
		return Compaction{Summary: "S", FirstKept: cut}, true, nil
	}))
	opts.Tools = []Tool{&fakeTool{name: "t"}}
	h := openWith(t, ctx, opts)
	defer h.Close()
	_, err := h.Prompt(ctx, "a", "msg a")
	require.NoError(t, err)
	sub, err := h.Prompt(ctx, "b", "msg b")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
	require.Equal(t, 2, compactionRows(t, h))
}

func TestCancelAfterCompactionLeavesValidHistory(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	tool := newBlockingTool("t")
	model := rec.model(func(n int, _ []Message) (Message, error) {
		if n == 1 {
			return okReply(n, nil)
		}
		return Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "t", Input: []byte(`{}`)}}}, nil
	})
	opts := fastOpts(model, rec.compactor(cutAtLast("S")))
	opts.Tools = []Tool{tool}
	h := openWith(t, ctx, opts)
	defer h.Close()
	_, err := h.Prompt(ctx, "a", "msg a")
	require.NoError(t, err)

	_, err = h.Submit(ctx, "b", "msg b")
	require.NoError(t, err)
	stop := startRun(h)
	<-tool.entered
	require.ErrorIs(t, stop(), context.Canceled)
	sub, err := h.Cancel(ctx, "b")
	require.NoError(t, err)
	require.Equal(t, StatusCancelled, sub.Status)

	hist, err := h.History(ctx)
	require.NoError(t, err)
	assertToolResultsMatch(t, hist)
	require.Equal(t, RequestCancelled, hist[len(hist)-1].Text)
	require.Equal(t, 1, compactionRows(t, h))
}

func TestIsOverflowText(t *testing.T) {
	cases := map[string]bool{
		"prompt is too long: 250000 tokens > 200000 maximum": true,
		"Request_Too_Large":                                             true,
		"Input is too long for requested model":                         true,
		"Your input exceeds the context window of this model":           true,
		"This model's maximum context length is 8192 tokens":            true,
		"input exceeds the model's maximum context length":              true,
		"Context window exceeded":                                       true,
		"The input token count (1200000) exceeds the maximum (1048575)": true,
		"maximum prompt length is 131072":                               true,
		"Please reduce the length of the messages":                      true,
		"exceeds the maximum allowed input length of 128,000 tokens":    true,
		"input is too large for model with 4096 maximum context length": true,
		"error code: context_length_exceeded":                           true,
		"context length exceeded":                                       true,
		"too many tokens in request":                                    true,
		"token limit exceeded":                                          true,
		"rate limit exceeded: too many tokens":                          false,
		"Too many requests, please slow down":                           false,
		"Throttling error: Too many tokens, please wait":                false,
		"Service unavailable: prompt is too long":                       false,
		"internal server error":                                         false,
		"":                                                              false,
	}
	for text, want := range cases {
		require.Equal(t, want, IsOverflowText(text), text)
	}
}

func TestContextOverflowHelper(t *testing.T) {
	require.NoError(t, ContextOverflow(nil))
	base := errors.New("x")
	err := ContextOverflow(base)
	require.True(t, isOverflow(err))
	require.ErrorIs(t, err, base)
	require.False(t, isOverflow(base))
	require.False(t, isTransient(err))
}

func TestGateRewrittenInputNeverReachesCompactor(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	opts := fastOpts(gateModel(), rec.compactor(func(int, CompactRequest) (Compaction, bool, error) {
		return Compaction{}, false, nil
	}))
	opts.Tools = []Tool{&fakeTool{name: "write_file"}}
	opts.Gate = rewriteGate(`{"token":"gate-secret"}`)
	h := openWith(t, ctx, opts)
	defer h.Close()

	sub, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)
	require.Equal(t, StatusDone, sub.Status)
	require.NotEmpty(t, rec.compacts)
	for _, req := range rec.compacts {
		body, err := json.Marshal(req.Messages)
		require.NoError(t, err)
		require.NotContains(t, string(body), "gate-secret")
	}
}
