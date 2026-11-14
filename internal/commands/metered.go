package commands

import (
	"context"
	"time"

	"github.com/wolfeidau/durable"
)

// meteredModel stamps each reply with its model, window, duration and, when the provider sent no cost, an estimate.
// Compaction summary calls use another model and are not metered.
type meteredModel struct {
	inner durable.Model
	model string
	info  modelInfo
}

func (m meteredModel) Generate(ctx context.Context, history []durable.Message, tools []durable.ToolSpec) (durable.Message, error) {
	start := time.Now()
	msg, err := m.inner.Generate(ctx, history, tools)
	if err != nil {
		return msg, err
	}
	if msg.Usage == nil {
		msg.Usage = &durable.Usage{}
	}
	u := msg.Usage
	u.DurationMs = time.Since(start).Milliseconds()
	u.Model = m.model
	u.ContextWindow = m.info.ContextWindow
	if u.CostSource == "" && m.info.Price != nil {
		u.CostUSD = m.info.Price.estimate(u)
		u.CostSource = durable.CostEstimate
	}
	return msg, nil
}

// estimate prices u at the tier with the highest threshold its prompt reaches.
func (p *modelPrice) estimate(u *durable.Usage) float64 {
	r := p.Base
	prompt := u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
	var best int64 = -1
	for _, t := range p.Overrides {
		if t.MinPromptTokens <= prompt && t.MinPromptTokens > best {
			r, best = t.Rates, t.MinPromptTokens
		}
	}
	return r.Prompt*float64(u.InputTokens) + r.CacheRead*float64(u.CacheReadTokens) +
		r.CacheWrite*float64(u.CacheWriteTokens) + r.Completion*float64(u.OutputTokens)
}
