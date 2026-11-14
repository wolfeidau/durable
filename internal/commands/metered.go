package commands

import (
	"context"
	"time"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/catalog"
)

// meteredModel stamps each reply with its model, window, duration and, when the provider sent no cost, an estimate.
// Compaction summary calls use another model and are not metered.
type meteredModel struct {
	inner durable.Model
	model string
	// info is not ready until the background lookup lands.
	info func() (catalog.Info, bool)
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
	if info, ok := m.info(); ok {
		u.ContextWindow = info.ContextWindow
		if u.CostSource == "" && info.Price != nil {
			u.CostUSD = info.Price.Estimate(u)
			u.CostSource = durable.CostEstimate
		}
	}
	return msg, nil
}
