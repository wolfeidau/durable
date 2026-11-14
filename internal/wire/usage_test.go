package wire

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
)

func TestEventMapsUsageAndAt(t *testing.T) {
	got := Event(durable.Event{
		Kind: durable.EventMessage,
		At:   1_700_000_000_123,
		Message: &durable.Message{Role: durable.RoleAssistant, Usage: &durable.Usage{
			InputTokens: 1, CacheReadTokens: 2, CacheWriteTokens: 3, OutputTokens: 4, ReasoningTokens: 5,
			CostUSD: 0.25, CostSource: durable.CostEstimate, Model: "m", ContextWindow: 6, DurationMs: 7,
		}},
	}, "ep")
	require.EqualValues(t, 1_700_000_000, got.At.GetSeconds())
	require.EqualValues(t, 123_000_000, got.At.GetNanos())
	u := got.Message.Usage
	require.EqualValues(t, 1, u.InputTokens)
	require.EqualValues(t, 2, u.CacheReadTokens)
	require.EqualValues(t, 3, u.CacheWriteTokens)
	require.EqualValues(t, 4, u.OutputTokens)
	require.EqualValues(t, 5, u.ReasoningTokens)
	require.InDelta(t, 0.25, u.CostUsd, 1e-12)
	require.Equal(t, "estimate", u.CostSource)
	require.Equal(t, "m", u.Model)
	require.EqualValues(t, 6, u.ContextWindow)
	require.EqualValues(t, 7, u.DurationMs)
}

func TestEventWithoutUsageOrAtLeavesThemUnset(t *testing.T) {
	got := Event(durable.Event{Kind: durable.EventMessage, Message: &durable.Message{Role: durable.RoleUser}}, "ep")
	require.Nil(t, got.At)
	require.Nil(t, got.Message.Usage)
}
