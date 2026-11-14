package commands

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/catalog"
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

var haikuPrice = &catalog.Price{
	Base:  catalog.Rates{Prompt: 1e-7, Completion: 5e-7, CacheRead: 1e-8, CacheWrite: 1.25e-7},
	Tiers: []catalog.Tier{{MinPromptTokens: 100000, Rates: catalog.Rates{Prompt: 5e-7, Completion: 2.5e-6, CacheRead: 5e-8, CacheWrite: 6.25e-7}}},
}

func ready(info catalog.Info) func() (catalog.Info, bool) {
	return func() (catalog.Info, bool) { return info, true }
}

func TestMeteredModelEstimatesOnceTheLookupLands(t *testing.T) {
	usage := durable.Usage{InputTokens: 1000, CacheReadTokens: 2000, CacheWriteTokens: 400, OutputTokens: 100}
	var info atomic.Pointer[catalog.Info]
	m := meteredModel{inner: replyModel{msg: durable.Message{Role: durable.RoleAssistant, Usage: &usage}}, model: "m", info: func() (catalog.Info, bool) {
		if i := info.Load(); i != nil {
			return *i, true
		}
		return catalog.Info{}, false
	}}

	got, err := m.Generate(context.Background(), nil, nil)
	require.NoError(t, err)
	require.Equal(t, "m", got.Usage.Model)
	require.Zero(t, got.Usage.ContextWindow)
	require.Empty(t, got.Usage.CostSource)
	require.Zero(t, got.Usage.CostUSD)

	info.Store(&catalog.Info{ContextWindow: 500, Price: haikuPrice})
	usage = durable.Usage{InputTokens: 1000, CacheReadTokens: 2000, CacheWriteTokens: 400, OutputTokens: 100}
	got, err = m.Generate(context.Background(), nil, nil)
	require.NoError(t, err)
	require.EqualValues(t, 500, got.Usage.ContextWindow)
	require.Equal(t, durable.CostEstimate, got.Usage.CostSource)
	require.InDelta(t, 1000*1e-7+2000*1e-8+400*1.25e-7+100*5e-7, got.Usage.CostUSD, 1e-12)
}

func TestMeteredModelStampsMetadataAndTimes(t *testing.T) {
	m := meteredModel{inner: replyModel{delay: 15 * time.Millisecond, msg: durable.Message{Role: durable.RoleAssistant, Text: "hi"}}, model: "vendor/m", info: ready(catalog.Info{ContextWindow: 500})}
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
	m := meteredModel{inner: replyModel{msg: durable.Message{Usage: &u}}, model: "m", info: ready(catalog.Info{Price: haikuPrice})}
	got, err := m.Generate(context.Background(), nil, nil)
	require.NoError(t, err)
	require.InDelta(t, 0.42, got.Usage.CostUSD, 1e-12)
	require.Equal(t, durable.CostProvider, got.Usage.CostSource)
}

func TestMeteredModelReturnsErrorsUnchanged(t *testing.T) {
	boom := durable.Transient(errors.New("boom"))
	m := meteredModel{inner: replyModel{err: boom}, model: "m", info: ready(catalog.Info{Price: haikuPrice})}
	got, err := m.Generate(context.Background(), nil, nil)
	require.Equal(t, boom, err)
	require.Nil(t, got.Usage)
}
