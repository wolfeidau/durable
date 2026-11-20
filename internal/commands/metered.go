package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/catalog"
)

// meteredModel stamps each reply with its model, window, duration and, when the provider sent no cost, an estimate.
// Compaction summary calls use another model and are not metered.
type meteredModel struct {
	inner    durable.Model
	model    string
	provider string
	// info is not ready until the background lookup lands.
	info func() (catalog.Info, bool)
	// tracer is nil for no spans.
	tracer         trace.Tracer
	captureContent bool
}

func (m meteredModel) Generate(ctx context.Context, history []durable.Message, tools []durable.ToolSpec) (durable.Message, error) {
	tracer := m.tracer
	if tracer == nil {
		tracer = durable.Tracer(nil)
	}
	ctx, span := tracer.Start(ctx, "chat "+m.model, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.provider.name", m.provider),
		attribute.String("gen_ai.request.model", m.model),
	))
	defer span.End()

	start := time.Now()
	msg, err := m.inner.Generate(ctx, history, tools)
	if err != nil {
		span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%T", err)))
		durable.SpanError(ctx, span, err)
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
	m.traceReply(span, msg)
	return msg, nil
}

func (m meteredModel) traceReply(span trace.Span, msg durable.Message) {
	u := msg.Usage
	span.SetAttributes(
		attribute.String("gen_ai.response.model", u.Model),
		attribute.Int64("gen_ai.usage.input_tokens", u.InputTokens),
		attribute.Int64("gen_ai.usage.output_tokens", u.OutputTokens),
	)
	if u.CacheReadTokens > 0 {
		span.SetAttributes(attribute.Int64("gen_ai.usage.cache_read.input_tokens", u.CacheReadTokens))
	}
	if u.CacheWriteTokens > 0 {
		span.SetAttributes(attribute.Int64("gen_ai.usage.cache_creation.input_tokens", u.CacheWriteTokens))
	}
	if u.CostSource != "" {
		span.SetAttributes(attribute.Float64("durable.cost_usd", u.CostUSD), attribute.String("durable.cost_source", u.CostSource))
	}
	if m.captureContent {
		durable.SetContent(span, "gen_ai.output.messages", outputMessagesJSON(msg))
	}
}

type outputPart struct {
	Type      string          `json:"type"`
	Content   string          `json:"content,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// outputMessagesJSON is the gen_ai.output.messages form of the reply alone, never the history.
func outputMessagesJSON(msg durable.Message) string {
	var parts []outputPart
	if msg.Text != "" {
		parts = append(parts, outputPart{Type: "text", Content: msg.Text})
	}
	for _, c := range msg.ToolCalls {
		parts = append(parts, outputPart{Type: "tool_call", ID: c.ID, Name: c.Name, Arguments: c.Input})
	}
	b, _ := json.Marshal([]struct {
		Role  string       `json:"role"`
		Parts []outputPart `json:"parts"`
	}{{Role: "assistant", Parts: parts}})
	return string(b)
}
