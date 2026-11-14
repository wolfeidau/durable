package commands

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
)

type replyModel struct {
	delay time.Duration
	msg   durable.Message
	err   error
}

func (m replyModel) Generate(context.Context, []durable.Message, []durable.ToolSpec) (durable.Message, error) {
	time.Sleep(m.delay)
	return m.msg, m.err
}

var haikuPrice = &modelPrice{
	Base: rates{Prompt: 1e-7, Completion: 5e-7, CacheRead: 1e-8, CacheWrite: 1.25e-7},
	Overrides: []priceTier{
		{MinPromptTokens: 100000, Rates: rates{Prompt: 5e-7, Completion: 2.5e-6, CacheRead: 5e-8, CacheWrite: 6.25e-7}},
		{MinPromptTokens: 50000, Rates: rates{Prompt: 3e-7, Completion: 1e-6, CacheRead: 3e-8, CacheWrite: 3e-7}},
	},
}

func TestMeteredModelEstimates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage durable.Usage
		want  float64
	}{
		{"base tier", durable.Usage{InputTokens: 1000, CacheReadTokens: 2000, CacheWriteTokens: 400, OutputTokens: 100},
			1000*1e-7 + 2000*1e-8 + 400*1.25e-7 + 100*5e-7},
		{"highest reached tier wins", durable.Usage{InputTokens: 10, CacheReadTokens: 120000, OutputTokens: 100},
			10*5e-7 + 120000*5e-8 + 100*2.5e-6},
		{"middle tier counts the whole prompt", durable.Usage{InputTokens: 20000, CacheReadTokens: 30000, CacheWriteTokens: 0, OutputTokens: 10},
			20000*3e-7 + 30000*3e-8 + 10*1e-6},
		{"threshold is inclusive", durable.Usage{InputTokens: 100000, OutputTokens: 1}, 100000*5e-7 + 2.5e-6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := meteredModel{inner: replyModel{msg: durable.Message{Role: durable.RoleAssistant, Usage: &tc.usage}}, model: "m", info: modelInfo{ContextWindow: 500, Price: haikuPrice}}
			got, err := m.Generate(context.Background(), nil, nil)
			require.NoError(t, err)
			require.InDelta(t, tc.want, got.Usage.CostUSD, 1e-12)
			require.Equal(t, durable.CostEstimate, got.Usage.CostSource)
		})
	}
}

func TestMeteredModelStampsMetadataAndTimes(t *testing.T) {
	m := meteredModel{inner: replyModel{delay: 15 * time.Millisecond, msg: durable.Message{Role: durable.RoleAssistant, Text: "hi"}}, model: "vendor/m", info: modelInfo{ContextWindow: 500}}
	got, err := m.Generate(context.Background(), nil, nil)
	require.NoError(t, err)
	require.NotNil(t, got.Usage, "allocated when the adapter reported none")
	require.GreaterOrEqual(t, got.Usage.DurationMs, int64(15))
	require.Equal(t, "vendor/m", got.Usage.Model)
	require.EqualValues(t, 500, got.Usage.ContextWindow)
	require.Empty(t, got.Usage.CostSource, "no price, so the cost stays unknown")
	require.Zero(t, got.Usage.CostUSD)
}

func TestMeteredModelKeepsProviderCost(t *testing.T) {
	u := durable.Usage{InputTokens: 1000, OutputTokens: 100, CostUSD: 0.42, CostSource: durable.CostProvider}
	m := meteredModel{inner: replyModel{msg: durable.Message{Usage: &u}}, model: "m", info: modelInfo{Price: haikuPrice}}
	got, err := m.Generate(context.Background(), nil, nil)
	require.NoError(t, err)
	require.InDelta(t, 0.42, got.Usage.CostUSD, 1e-12)
	require.Equal(t, durable.CostProvider, got.Usage.CostSource)
}

func TestMeteredModelReturnsErrorsUnchanged(t *testing.T) {
	boom := durable.Transient(errors.New("boom"))
	m := meteredModel{inner: replyModel{err: boom}, model: "m", info: modelInfo{Price: haikuPrice}}
	got, err := m.Generate(context.Background(), nil, nil)
	require.Equal(t, boom, err)
	require.Nil(t, got.Usage)
}
