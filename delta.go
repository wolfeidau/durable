package durable

import "context"

// DeltaKind says what a Delta carries.
type DeltaKind string

const (
	// DeltaReset starts a model attempt, so a consumer drops any partial text from an earlier one.
	DeltaReset DeltaKind = "reset"
	// DeltaText is a piece of the assistant's reply.
	DeltaText DeltaKind = "text"
	// DeltaToolOutput is a piece of a running tool's output.
	DeltaToolOutput DeltaKind = "tool_output"
)

// Delta is a live, best-effort view of work in progress. Deltas are never persisted.
// Drop partial text on DeltaReset, the assistant message event or a terminal submission event, and partial tool output on call_done.
type Delta struct {
	Kind       DeltaKind
	Submission int64
	CallID     string // DeltaToolOutput only
	Text       string
}

type sinkKey struct{}

type sink struct {
	fn         func(Delta)
	submission int64
	callID     string
	noOutput   bool
}

func withSink(ctx context.Context, fn func(Delta), submission int64, callID string) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, sinkKey{}, &sink{fn: fn, submission: submission, callID: callID})
}

func sinkFrom(ctx context.Context) *sink {
	s, _ := ctx.Value(sinkKey{}).(*sink)
	return s
}

// EmitText reports a piece of the assistant's reply. It does nothing outside a harness model call.
func EmitText(ctx context.Context, s string) {
	if sk := sinkFrom(ctx); sk != nil && s != "" {
		sk.fn(Delta{Kind: DeltaText, Submission: sk.submission, Text: s})
	}
}

// EmitToolOutput reports a piece of a running tool's output. It does nothing outside a harness tool call,
// or under WithoutToolOutput.
func EmitToolOutput(ctx context.Context, s string) {
	if sk := sinkFrom(ctx); sk != nil && !sk.noOutput && s != "" {
		sk.fn(Delta{Kind: DeltaToolOutput, Submission: sk.submission, CallID: sk.callID, Text: s})
	}
}

// WithoutToolOutput blocks EmitToolOutput on the returned context, for wrappers that may rewrite or withhold
// the result. Text is not affected.
func WithoutToolOutput(ctx context.Context) context.Context {
	sk := sinkFrom(ctx)
	if sk == nil {
		return ctx
	}
	cp := *sk
	cp.noOutput = true
	return context.WithValue(ctx, sinkKey{}, &cp)
}

// WithToolOutputSink makes EmitToolOutput on the returned context call fn, so a tool host outside the harness can forward live output.
func WithToolOutputSink(ctx context.Context, fn func(string)) context.Context {
	if fn == nil {
		return ctx
	}
	return withSink(ctx, func(d Delta) { fn(d.Text) }, 0, "")
}

// ToolOutputEnabled lets a remote tool skip asking for output that EmitToolOutput would drop.
func ToolOutputEnabled(ctx context.Context) bool {
	sk := sinkFrom(ctx)
	return sk != nil && !sk.noOutput
}

func emitReset(ctx context.Context) {
	if sk := sinkFrom(ctx); sk != nil {
		sk.fn(Delta{Kind: DeltaReset, Submission: sk.submission})
	}
}
