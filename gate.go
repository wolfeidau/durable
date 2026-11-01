package durable

import (
	"context"
	"encoding/json"
	"fmt"
)

// Action is what a ToolGate decides for a pending tool call.
type Action int

const (
	// ActionAsk is the zero value so an empty Decision can never allow a call.
	ActionAsk Action = iota
	ActionAllow
	ActionDeny
)

// Decision is a ToolGate's verdict on one tool call.
type Decision struct {
	Action Action
	// Reason is the tool result recorded for a deny; empty uses Denied.
	Reason string
	// Input, if non-nil, replaces the model's input. Invalid JSON denies the call with InvalidRewrite.
	Input json.RawMessage
}

// ToolGate decides whether a pending tool call may run. An error or cancelled
// context leaves the call pending for the next Run.
type ToolGate func(ctx context.Context, call ToolCall) (Decision, error)

// ReadOnlyTool is an optional Tool extension for tools that never change state.
// Wrappers must forward it, like Recoverer.
type ReadOnlyTool interface {
	ReadOnly() bool
}

// ReadOnlyGate allows the read-only tools and asks for the rest, so a new tool asks by default.
func ReadOnlyGate(tools []Tool) ToolGate {
	readOnly := map[string]bool{}
	for _, t := range tools {
		if r, ok := t.(ReadOnlyTool); ok && r.ReadOnly() {
			readOnly[t.Spec().Name] = true
		}
	}
	return func(_ context.Context, call ToolCall) (Decision, error) {
		if readOnly[call.Name] {
			return Decision{Action: ActionAllow}, nil
		}
		return Decision{Action: ActionAsk}, nil
	}
}

// decide runs Gate then Ask. It returns the call to execute and, when the call
// is blocked, the result to record for it.
func (h *Harness) decide(ctx context.Context, call ToolCall) (ToolCall, string, bool, error) {
	d := Decision{Action: ActionAsk}
	if h.opts.Gate != nil {
		var err error
		d, err = h.opts.Gate(ctx, call)
		if err != nil {
			return call, "", false, fmt.Errorf("failed to gate tool call %q: %w", call.ID, err)
		}
		if ctx.Err() != nil {
			return call, "", false, ctx.Err()
		}
	} else if h.opts.Ask == nil {
		d.Action = ActionAllow
	}

	switch d.Action {
	case ActionAllow, ActionAsk:
	default:
		d.Action = ActionDeny
	}
	if d.Action == ActionDeny {
		reason := d.Reason
		if reason == "" {
			reason = Denied
		}
		return call, reason, true, nil
	}

	if d.Input != nil {
		if !json.Valid(d.Input) {
			// Retrying can't fix this, and a pending call would block the whole queue.
			return call, InvalidRewrite, true, nil
		}
		call.Input = d.Input
	}

	if d.Action == ActionAsk {
		if h.opts.Ask == nil {
			return call, Denied, true, nil
		}
		ok, err := h.opts.Ask(ctx, call)
		if err != nil {
			return call, "", false, fmt.Errorf("failed to approve tool call %q: %w", call.ID, err)
		}
		if ctx.Err() != nil {
			return call, "", false, ctx.Err()
		}
		if !ok {
			return call, Declined, true, nil
		}
	}
	return call, "", false, nil
}
