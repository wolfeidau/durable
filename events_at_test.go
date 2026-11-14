package durable

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEventsCarryAtAndUsageRoundTrips(t *testing.T) {
	ctx := context.Background()
	usage := &Usage{InputTokens: 3, CacheReadTokens: 4, OutputTokens: 5, CostUSD: 0.5, CostSource: CostProvider, Model: "m", ContextWindow: 100, DurationMs: 7}
	m := modelFunc(func(context.Context, []Message, []ToolSpec) (Message, error) {
		return Message{Role: RoleAssistant, Text: "ok", Usage: usage}, nil
	})
	h := openWith(t, ctx, Options{Model: m, LeaseTTL: ttl})
	t.Cleanup(func() { _ = h.Close() })

	before := time.Now().UnixMilli()
	_, err := h.Prompt(ctx, "a", "go")
	require.NoError(t, err)

	events, err := h.Events(ctx, 0, 0)
	require.NoError(t, err)
	require.NotEmpty(t, events)
	var replies int
	for _, e := range events {
		require.GreaterOrEqual(t, e.At, before)
		if e.Kind == EventMessage && e.Message.Role == RoleAssistant {
			replies++
			require.Equal(t, usage, e.Message.Usage)
		}
	}
	require.Equal(t, 1, replies)

	history, err := h.History(ctx)
	require.NoError(t, err)
	require.Equal(t, usage, history[len(history)-1].Usage)
}

func TestOldEventAndMessageJSONDecode(t *testing.T) {
	var e Event
	require.NoError(t, json.Unmarshal([]byte(`{"kind":"message","submission":1,"message":{"role":"assistant","text":"hi","context_tokens":9}}`), &e))
	require.Zero(t, e.At)
	require.Nil(t, e.Message.Usage)
	require.EqualValues(t, 9, e.Message.ContextTokens)
}
