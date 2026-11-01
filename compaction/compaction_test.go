package compaction

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
)

type fakeModel struct {
	reply durable.Message
	err   error
	calls [][]durable.Message
	tools [][]durable.ToolSpec
}

func (f *fakeModel) Generate(_ context.Context, history []durable.Message, tools []durable.ToolSpec) (durable.Message, error) {
	f.calls = append(f.calls, history)
	f.tools = append(f.tools, tools)
	return f.reply, f.err
}

func text(role durable.Role, chars int) durable.Message {
	return durable.Message{Role: role, Text: strings.Repeat("x", chars)}
}

// convo builds n alternating user/assistant messages of the given size.
func convo(n, chars int) []durable.Message {
	msgs := make([]durable.Message, n)
	for i := range msgs {
		role := durable.RoleUser
		if i%2 == 1 {
			role = durable.RoleAssistant
		}
		msgs[i] = text(role, chars)
	}
	return msgs
}

func newFake() *fakeModel { return &fakeModel{reply: durable.Message{Text: "  summary  "}} }

func TestCompactThreshold(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		measured int64
		force    bool
		want     bool
	}{
		{"below threshold", Config{ContextWindow: 1000, ReserveTokens: 100}, 900, false, false},
		{"above threshold", Config{ContextWindow: 1000, ReserveTokens: 100}, 901, false, true},
		{"window zero no force", Config{}, 100000, false, false},
		{"window zero force", Config{KeepTokens: 500}, 100000, true, true},
		{"force below threshold", Config{ContextWindow: 1000, ReserveTokens: 100}, 10, true, true},
		{"reserve equals window clamps to half", Config{ContextWindow: 1000, ReserveTokens: 1000}, 501, false, true},
		{"reserve above window below half", Config{ContextWindow: 1000, ReserveTokens: 5000}, 500, false, false},
		{"default reserve exceeds small window", Config{ContextWindow: 8000}, 4001, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			tc.cfg.Model = f
			msgs := convo(40, 400)
			msgs[len(msgs)-1].ContextTokens = tc.measured

			c, ok, err := New(tc.cfg).Compact(t.Context(), durable.CompactRequest{Messages: msgs, Force: tc.force})
			require.NoError(t, err)
			require.Equal(t, tc.want, ok)
			if tc.want {
				require.Equal(t, "summary", c.Summary)
				require.Positive(t, c.FirstKept)
				require.Less(t, c.FirstKept, len(msgs))
				require.Len(t, f.calls, 1)
				require.Nil(t, f.tools[0])
			} else {
				require.Empty(t, f.calls)
			}
		})
	}
}

func TestEstimate(t *testing.T) {
	c := New(Config{ContextWindow: 1000, ReserveTokens: 100})

	t.Run("measured plus trailing chars", func(t *testing.T) {
		msgs := convo(4, 40)
		msgs[1].ContextTokens = 500
		msgs[3].ContextTokens = 0
		// newest non-zero is index 1; messages 2 and 3 add 80 chars = 20 tokens
		require.Equal(t, int64(520), c.estimate(durable.CompactRequest{Messages: msgs}))
	})
	t.Run("newest measured wins", func(t *testing.T) {
		msgs := convo(4, 40)
		msgs[0].ContextTokens = 900
		msgs[2].ContextTokens = 100
		require.Equal(t, int64(110), c.estimate(durable.CompactRequest{Messages: msgs}))
	})
	t.Run("fallback rounds up and ignores raw", func(t *testing.T) {
		msgs := []durable.Message{{Role: durable.RoleUser, Text: "abcde", Raw: json.RawMessage(strings.Repeat("z", 1000))}}
		require.Equal(t, int64(2), c.estimate(durable.CompactRequest{Messages: msgs}))
	})
	t.Run("fallback counts tool calls and previous", func(t *testing.T) {
		msgs := []durable.Message{{
			Role:      durable.RoleAssistant,
			ToolCalls: []durable.ToolCall{{Name: "bash", Input: json.RawMessage(`{"a":1}`)}},
		}}
		require.Equal(t, int64(3), c.estimate(durable.CompactRequest{Messages: msgs}))
		require.Equal(t, int64(3+10), c.estimate(durable.CompactRequest{Messages: msgs, Previous: strings.Repeat("p", 40)}))
	})
	t.Run("previous ignored when measured", func(t *testing.T) {
		msgs := convo(2, 40)
		msgs[1].ContextTokens = 50
		require.Equal(t, int64(50), c.estimate(durable.CompactRequest{Messages: msgs, Previous: strings.Repeat("p", 4000)}))
	})
}

func TestCutValidity(t *testing.T) {
	calls := []durable.ToolCall{
		{ID: "1", Name: "a", Input: json.RawMessage(`{}`)},
		{ID: "2", Name: "b", Input: json.RawMessage(`{}`)},
		{ID: "3", Name: "c", Input: json.RawMessage(`{}`)},
	}
	tool := func(id string) durable.Message {
		return durable.Message{Role: durable.RoleTool, ToolCallID: id, Text: strings.Repeat("r", 40)}
	}
	withCalls := durable.Message{Role: durable.RoleAssistant, ToolCalls: calls, Text: strings.Repeat("a", 40)}

	shapes := map[string][]durable.Message{
		"parallel calls mid-history": {
			text(durable.RoleUser, 40), text(durable.RoleAssistant, 40), text(durable.RoleUser, 40),
			withCalls, tool("1"), tool("2"), tool("3"), text(durable.RoleAssistant, 40), text(durable.RoleUser, 40),
		},
		"parallel calls at the end": {
			text(durable.RoleUser, 40), text(durable.RoleAssistant, 40), text(durable.RoleUser, 40),
			withCalls, tool("1"), tool("2"), tool("3"),
		},
		"tools after the first message": {
			text(durable.RoleUser, 40), tool("1"), tool("2"), tool("3"),
		},
	}
	for name, msgs := range shapes {
		for _, force := range []bool{false, true} {
			for keep := int64(1); keep <= 80; keep++ {
				c := New(Config{Model: newFake(), KeepTokens: keep})
				got, ok, err := c.Compact(t.Context(), durable.CompactRequest{Messages: msgs, Force: force})
				require.NoError(t, err, name)
				if !ok {
					continue
				}
				require.Positive(t, got.FirstKept, "%s keep=%d force=%v", name, keep, force)
				require.Less(t, got.FirstKept, len(msgs), "%s keep=%d force=%v", name, keep, force)
				require.NotEqual(t, durable.RoleTool, msgs[got.FirstKept].Role, "%s keep=%d force=%v", name, keep, force)
			}
		}
	}
}

func TestCutSelection(t *testing.T) {
	tests := []struct {
		name  string
		msgs  []durable.Message
		keep  int64
		force bool
		want  int // -1 for ok=false
	}{
		{"keeps newest by budget and prefers user", convo(10, 40), 25, false, 8},
		{"budget lands on user keeps it", convo(10, 40), 35, false, 6},
		{"assistant when no later user", convo(8, 40), 5, false, 7},
		{"budget never reached", convo(4, 40), 1000, false, -1},
		{"budget never reached with force", convo(4, 40), 1000, true, 3},
		{"only candidate is index 0", convo(4, 40), 40, false, -1},
		{"only candidate is index 0 with force", convo(4, 40), 40, true, 3},
		{"single message", convo(1, 40), 1, false, -1},
		{"single message with force", convo(1, 40), 1, true, -1},
		{"nil messages", nil, 1, false, -1},
		{"nil messages with force", nil, 1, true, -1},
		{"empty messages", []durable.Message{}, 1, false, -1},
		{"empty messages with force", []durable.Message{}, 1, true, -1},
		{
			"all tools after index 0",
			[]durable.Message{text(durable.RoleUser, 40), text(durable.RoleTool, 40), text(durable.RoleTool, 40)},
			1, false, -1,
		},
		{
			"all tools after index 0 with force",
			[]durable.Message{text(durable.RoleUser, 40), text(durable.RoleTool, 40), text(durable.RoleTool, 40)},
			1, true, -1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			c := New(Config{Model: f, KeepTokens: tc.keep, ContextWindow: 1 << 40, ReserveTokens: 1})
			if n := len(tc.msgs); n > 0 {
				tc.msgs[n-1].ContextTokens = 1 << 41
			}
			got, ok, err := c.Compact(t.Context(), durable.CompactRequest{Messages: tc.msgs, Force: tc.force})
			require.NoError(t, err)
			if tc.want < 0 {
				require.False(t, ok)
				require.Empty(t, f.calls)
				return
			}
			require.True(t, ok)
			require.Equal(t, tc.want, got.FirstKept)
		})
	}
}

func TestCutKeepClampedForSmallWindow(t *testing.T) {
	// Window 2000, reserve 1500 gives threshold 500 and keep 250, not the 20000 default.
	f := newFake()
	c := New(Config{Model: f, ContextWindow: 2000, ReserveTokens: 1500})
	msgs := convo(20, 400) // 100 tokens each
	msgs[19].ContextTokens = 600
	got, ok, err := c.Compact(t.Context(), durable.CompactRequest{Messages: msgs})
	require.NoError(t, err)
	require.True(t, ok)
	// 250 tokens reached at index 17; nearest user at or after is 18.
	require.Equal(t, 18, got.FirstKept)
}

func TestSerialization(t *testing.T) {
	t.Run("labels and joins", func(t *testing.T) {
		msgs := []durable.Message{
			{Role: durable.RoleUser, Text: "hi"},
			{Role: durable.RoleAssistant, Text: "calling", ToolCalls: []durable.ToolCall{
				{Name: "read", Input: json.RawMessage(`{"path":"a"}`)},
				{Name: "ls", Input: json.RawMessage(`{}`)},
			}},
			{Role: durable.RoleTool, Text: "contents"},
			{Role: durable.RoleTool, Text: "boom", IsError: true},
		}
		want := "[User]: hi\n\n[Assistant]: calling\n\n" +
			`[Assistant tool calls]: read({"path":"a"}); ls({})` +
			"\n\n[Tool result]: contents\n\n[Tool error]: boom"
		require.Equal(t, want, serialize(msgs))
	})
	t.Run("raw never appears", func(t *testing.T) {
		msgs := []durable.Message{
			{Role: durable.RoleUser, Text: "hi", Raw: json.RawMessage(`"SECRETRAW"`)},
			{Role: durable.RoleAssistant, Text: "ok", Raw: json.RawMessage(`"SECRETRAW"`)},
		}
		require.NotContains(t, buildPrompt("", msgs), "SECRETRAW")
	})
	t.Run("nil input tool call", func(t *testing.T) {
		msgs := []durable.Message{{Role: durable.RoleAssistant, ToolCalls: []durable.ToolCall{{Name: "noargs"}}}}
		require.Equal(t, "[Assistant tool calls]: noargs()", serialize(msgs))
	})
	t.Run("tool call input is serialized exactly", func(t *testing.T) {
		msgs := []durable.Message{{Role: durable.RoleAssistant, ToolCalls: []durable.ToolCall{
			{Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)},
		}}}
		got := serialize(msgs)
		require.Contains(t, got, `bash({"command":"ls"})`)
		require.NotContains(t, got, "token")
	})
	t.Run("tool results truncated by rune", func(t *testing.T) {
		long := strings.Repeat("é世", 1500) // 3000 runes
		got := serialize([]durable.Message{{Role: durable.RoleTool, Text: long}})
		want := "[Tool result]: " + string([]rune(long)[:2000]) + "\n\n[... 1000 more characters truncated]"
		require.Equal(t, want, got)
	})
	t.Run("tool result at limit is untouched", func(t *testing.T) {
		exact := strings.Repeat("世", 2000)
		require.Equal(t, "[Tool result]: "+exact, serialize([]durable.Message{{Role: durable.RoleTool, Text: exact}}))
	})
	t.Run("user and assistant text not truncated", func(t *testing.T) {
		long := strings.Repeat("u", 5000)
		got := serialize([]durable.Message{{Role: durable.RoleUser, Text: long}, {Role: durable.RoleAssistant, Text: long}})
		require.Equal(t, "[User]: "+long+"\n\n[Assistant]: "+long, got)
	})
}

func TestPrompt(t *testing.T) {
	msgs := convo(10, 40)

	t.Run("fresh", func(t *testing.T) {
		f := newFake()
		_, ok, err := New(Config{Model: f, KeepTokens: 25}).Compact(t.Context(), durable.CompactRequest{Messages: msgs, Force: true})
		require.NoError(t, err)
		require.True(t, ok)
		require.Len(t, f.calls, 1)
		require.Len(t, f.calls[0], 1)
		require.Equal(t, durable.RoleUser, f.calls[0][0].Role)
		p := f.calls[0][0].Text
		require.True(t, strings.HasPrefix(p, "<conversation>\n[User]: "))
		require.Contains(t, p, "</conversation>")
		require.Contains(t, p, summarizationPrompt)
		require.NotContains(t, p, "<previous-summary>")
	})
	t.Run("update", func(t *testing.T) {
		f := newFake()
		_, ok, err := New(Config{Model: f, KeepTokens: 25}).Compact(t.Context(), durable.CompactRequest{Messages: msgs, Previous: "OLD SUMMARY", Force: true})
		require.NoError(t, err)
		require.True(t, ok)
		p := f.calls[0][0].Text
		require.Contains(t, p, "<previous-summary>\nOLD SUMMARY\n</previous-summary>")
		require.Contains(t, p, updateIntro)
		require.NotContains(t, p, summarizationPrompt)
	})
	t.Run("only messages before the cut", func(t *testing.T) {
		f := newFake()
		msgs := convo(10, 40)
		msgs[9].Text = "NEWEST" + strings.Repeat("x", 34)
		got, ok, err := New(Config{Model: f, KeepTokens: 25}).Compact(t.Context(), durable.CompactRequest{Messages: msgs, Force: true})
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, 8, got.FirstKept)
		require.NotContains(t, f.calls[0][0].Text, "NEWEST")
	})
}

func TestModelErrors(t *testing.T) {
	msgs := convo(10, 40)
	req := durable.CompactRequest{Messages: msgs, Force: true}

	t.Run("wrapped permanent", func(t *testing.T) {
		cause := errors.New("bad request")
		_, ok, err := New(Config{Model: &fakeModel{err: cause}, KeepTokens: 25}).Compact(t.Context(), req)
		require.False(t, ok)
		require.ErrorIs(t, err, cause)
		require.ErrorContains(t, err, "bad request")
	})
	t.Run("transient survives wrapping", func(t *testing.T) {
		transient := durable.Transient(errors.New("rate limited"))
		_, ok, err := New(Config{Model: &fakeModel{err: transient}, KeepTokens: 25}).Compact(t.Context(), req)
		require.False(t, ok)
		require.ErrorIs(t, err, transient)
		require.NotEqual(t, transient, err)
	})
	t.Run("empty reply", func(t *testing.T) {
		for name, reply := range map[string]durable.Message{
			"zero message": {},
			"empty text":   {Text: ""},
			"whitespace":   {Text: " \n\t "},
		} {
			t.Run(name, func(t *testing.T) {
				_, ok, err := New(Config{Model: &fakeModel{reply: reply}, KeepTokens: 25}).Compact(t.Context(), req)
				require.Error(t, err)
				require.False(t, ok)
				require.ErrorContains(t, err, "empty summary")
			})
		}
	})
}

func TestDefaults(t *testing.T) {
	c := New(Config{})
	require.Equal(t, int64(16384), c.cfg.ReserveTokens)
	require.Equal(t, int64(20000), c.cfg.KeepTokens)

	c = New(Config{ReserveTokens: 5, KeepTokens: 7})
	require.Equal(t, int64(5), c.cfg.ReserveTokens)
	require.Equal(t, int64(7), c.cfg.KeepTokens)
}
