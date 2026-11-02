package durable

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrAwaitingApproval is returned by Run when a call is parked for Decide. The submission
// stays running and the call stays pending; call Run again after deciding.
var ErrAwaitingApproval = errors.New("durable: awaiting approval")

// ErrApprovalDecided means the approval already has a different answer, or was cancelled.
var ErrApprovalDecided = errors.New("durable: approval already decided")

// ApprovalStatus is where an approval is in its lifecycle.
type ApprovalStatus string

const (
	ApprovalPending   ApprovalStatus = "pending"
	ApprovalAllowed   ApprovalStatus = "allowed"
	ApprovalDeclined  ApprovalStatus = "declined"
	ApprovalCancelled ApprovalStatus = "cancelled" // the call finished without an answer
)

// Approval is a durable request to run one tool call.
type Approval struct {
	Seq        int64
	Submission int64
	// Call carries the input that runs if allowed, which can contain secrets a gate added.
	Call      ToolCall
	Rewritten bool // a gate changed Call.Input
	Status    ApprovalStatus
	DecidedBy string
}

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

// decide runs Gate then Ask, after checking for a stored approval. It returns the call to
// execute and, when the call is blocked, the result to record for it.
func (h *Harness) decide(ctx context.Context, sub Submission, c storedCall) (ToolCall, string, bool, error) {
	call := c.ToolCall
	stored, err := queryApproval(ctx, h.db, `a.message_seq = ? AND a.position = ?`, c.MessageSeq, c.Position)
	switch {
	case err == nil:
		call.Input = stored.Call.Input
		return h.resolveStored(ctx, call, stored)
	case !errors.Is(err, sql.ErrNoRows):
		return call, "", false, err
	}

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
	} else if h.opts.Ask == nil && !h.opts.DeferApprovals {
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
		return h.askCall(ctx, sub, c, call)
	}
	return call, "", false, nil
}

// askCall parks the call when approvals are deferred, and otherwise asks in place.
func (h *Harness) askCall(ctx context.Context, sub Submission, c storedCall, call ToolCall) (ToolCall, string, bool, error) {
	if h.opts.DeferApprovals {
		err := h.commit(ctx, func(tx *sql.Tx) error {
			return requestApproval(ctx, tx, sub, c, call)
		})
		if err != nil {
			return call, "", false, err
		}
		return call, "", false, ErrAwaitingApproval
	}
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
	return call, "", false, nil
}

// resolveStored acts on an approval row that already exists, so the Gate never runs twice.
func (h *Harness) resolveStored(ctx context.Context, call ToolCall, stored Approval) (ToolCall, string, bool, error) {
	if stored.Status == ApprovalPending {
		switch {
		case h.opts.DeferApprovals:
			return call, "", false, ErrAwaitingApproval
		case h.opts.Ask == nil:
			// Closed as cancelled by finishCall; never auto-allow.
			return call, Denied, true, nil
		}
		ok, err := h.opts.Ask(ctx, call)
		if err != nil {
			return call, "", false, fmt.Errorf("failed to approve tool call %q: %w", call.ID, err)
		}
		if ctx.Err() != nil {
			return call, "", false, ctx.Err()
		}
		err = h.commit(ctx, func(tx *sql.Tx) error {
			var err error
			stored, err = setApproval(ctx, tx, stored.Seq, ok, "ask")
			return err
		})
		// A concurrent Decide won; its answer stands.
		if err != nil && !errors.Is(err, ErrApprovalDecided) {
			return call, "", false, err
		}
	}
	switch stored.Status {
	case ApprovalAllowed:
		return call, "", false, nil
	case ApprovalDeclined:
		return call, Declined, true, nil
	default:
		return call, Denied, true, nil
	}
}

// PendingApproval returns the approval waiting for an answer, if any. There is at most one,
// because calls run one at a time.
func (h *Harness) PendingApproval(ctx context.Context) (Approval, bool, error) {
	a, err := queryApproval(ctx, h.db, `a.status = 'pending' ORDER BY a.seq LIMIT 1`)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, false, nil
	}
	return a, err == nil, err
}

// Decide records the answer to an approval; by says who answered and is not authenticated.
// Repeating the same answer is a no-op. A different answer, or one for a cancelled approval,
// returns the stored approval and ErrApprovalDecided. Call Run afterwards to continue.
func (h *Harness) Decide(ctx context.Context, seq int64, allow bool, by string) (Approval, error) {
	var a Approval
	err := h.commit(ctx, func(tx *sql.Tx) error {
		var err error
		a, err = setApproval(ctx, tx, seq, allow, by)
		return err
	})
	return a, err
}

func setApproval(ctx context.Context, tx *sql.Tx, seq int64, allow bool, by string) (Approval, error) {
	a, err := queryApproval(ctx, tx, `a.seq = ?`, seq)
	if err != nil {
		return Approval{}, fmt.Errorf("failed to find approval %d: %w", seq, err)
	}
	want := ApprovalDeclined
	if allow {
		want = ApprovalAllowed
	}
	if a.Status == want {
		return a, nil
	}
	if a.Status != ApprovalPending {
		return a, ErrApprovalDecided
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE approvals SET status = ?, decided_by = ?, decided_at = ? WHERE seq = ?`,
		want, by, nowMillis(), seq); err != nil {
		return a, fmt.Errorf("failed to record approval: %w", err)
	}
	a.Status, a.DecidedBy = want, by
	return a, addEvent(ctx, tx, Event{
		Kind: EventApprovalDecided, Submission: a.Submission, ApprovalSeq: seq,
		Approval: want, DecidedBy: by, Call: &ToolCall{ID: a.Call.ID, Name: a.Call.Name},
	})
}
