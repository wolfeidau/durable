package durable

import (
	"context"
	"encoding/json"
	"errors"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TracerName is the instrumentation scope of every span the harness and its callers create.
const TracerName = "github.com/wolfeidau/durable"

// maxContentBytes bounds any one content attribute, because prompts and tool output are unbounded.
const maxContentBytes = 16 << 10

// Tracer returns the durable tracer for tp, or a no-op one when tp is nil.
func Tracer(tp trace.TracerProvider) trace.Tracer {
	if tp == nil {
		tp = noop.NewTracerProvider()
	}
	return tp.Tracer(TracerName)
}

// WithSpanAttributes returns a provider whose tracers add attrs to every span they start. A nil tp stays nil.
func WithSpanAttributes(tp trace.TracerProvider, attrs ...attribute.KeyValue) trace.TracerProvider {
	if tp == nil || len(attrs) == 0 {
		return tp
	}
	return attrProvider{TracerProvider: tp, attrs: attrs}
}

type attrProvider struct {
	trace.TracerProvider
	attrs []attribute.KeyValue
}

func (p attrProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	return attrTracer{Tracer: p.TracerProvider.Tracer(name, opts...), attrs: p.attrs}
}

type attrTracer struct {
	trace.Tracer
	attrs []attribute.KeyValue
}

func (t attrTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return t.Tracer.Start(ctx, name, append([]trace.SpanStartOption{trace.WithAttributes(t.attrs...)}, opts...)...)
}

// SetContent sets a content attribute, cut to 16 KiB. A cut also sets durable.content_truncated on the span.
// Callers gate it on Options.CaptureContent.
func SetContent(span trace.Span, key, value string) {
	if len(value) > maxContentBytes {
		n := maxContentBytes
		for n > 0 && !utf8.RuneStart(value[n]) {
			n--
		}
		value = value[:n]
		span.SetAttributes(attribute.Bool("durable.content_truncated", true))
	}
	span.SetAttributes(attribute.String(key, value))
}

// SpanError marks span as failed unless err is nil, an approval park, or a cancellation. A lost lease is a
// failure even though it cancels ctx. It does not end the span.
func SpanError(ctx context.Context, span trace.Span, err error) {
	if err == nil || errors.Is(err, ErrAwaitingApproval) {
		return
	}
	if ctx.Err() != nil && !errors.Is(context.Cause(ctx), ErrLeaseLost) && !errors.Is(err, ErrLeaseLost) {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

type textPart struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

type genAIMessage struct {
	Role  string     `json:"role"`
	Parts []textPart `json:"parts"`
}

// userMessagesJSON is the gen_ai.input.messages form of a single user prompt.
func userMessagesJSON(text string) string {
	b, _ := json.Marshal([]genAIMessage{{Role: "user", Parts: []textPart{{Type: "text", Content: text}}}})
	return string(b)
}
