// Package wire converts durable types to the API types, redacting what a gate may have added.
package wire

import (
	"github.com/wolfeidau/durable"
	durablev1 "github.com/wolfeidau/durable/api/durable/v1"
)

var eventKinds = map[durable.EventKind]durablev1.EventKind{
	durable.EventSubmission:        durablev1.EventKind_EVENT_KIND_SUBMISSION,
	durable.EventMessage:           durablev1.EventKind_EVENT_KIND_MESSAGE,
	durable.EventCallStarted:       durablev1.EventKind_EVENT_KIND_CALL_STARTED,
	durable.EventCallReplayed:      durablev1.EventKind_EVENT_KIND_CALL_REPLAYED,
	durable.EventCallDone:          durablev1.EventKind_EVENT_KIND_CALL_DONE,
	durable.EventCompacted:         durablev1.EventKind_EVENT_KIND_COMPACTED,
	durable.EventCompactionFailed:  durablev1.EventKind_EVENT_KIND_COMPACTION_FAILED,
	durable.EventApprovalRequested: durablev1.EventKind_EVENT_KIND_APPROVAL_REQUESTED,
	durable.EventApprovalDecided:   durablev1.EventKind_EVENT_KIND_APPROVAL_DECIDED,
}

var submissionStatuses = map[durable.SubmissionStatus]durablev1.SubmissionStatus{
	durable.StatusQueued:    durablev1.SubmissionStatus_SUBMISSION_STATUS_QUEUED,
	durable.StatusRunning:   durablev1.SubmissionStatus_SUBMISSION_STATUS_RUNNING,
	durable.StatusDone:      durablev1.SubmissionStatus_SUBMISSION_STATUS_DONE,
	durable.StatusFailed:    durablev1.SubmissionStatus_SUBMISSION_STATUS_FAILED,
	durable.StatusCancelled: durablev1.SubmissionStatus_SUBMISSION_STATUS_CANCELLED,
}

var approvalStatuses = map[durable.ApprovalStatus]durablev1.ApprovalStatus{
	durable.ApprovalPending:   durablev1.ApprovalStatus_APPROVAL_STATUS_PENDING,
	durable.ApprovalAllowed:   durablev1.ApprovalStatus_APPROVAL_STATUS_ALLOWED,
	durable.ApprovalDeclined:  durablev1.ApprovalStatus_APPROVAL_STATUS_DECLINED,
	durable.ApprovalCancelled: durablev1.ApprovalStatus_APPROVAL_STATUS_CANCELLED,
}

var roles = map[durable.Role]durablev1.Role{
	durable.RoleUser:      durablev1.Role_ROLE_USER,
	durable.RoleAssistant: durablev1.Role_ROLE_ASSISTANT,
	durable.RoleTool:      durablev1.Role_ROLE_TOOL,
}

// Event converts e. A rewritten call loses its input, because a gate can only add secrets by
// rewriting and the model's own arguments are already in the assistant message.
func Event(e durable.Event, epoch string) *durablev1.Event {
	return &durablev1.Event{
		Seq:         e.Seq,
		Epoch:       epoch,
		Kind:        eventKinds[e.Kind],
		Submission:  e.Submission,
		Status:      submissionStatuses[e.Status],
		Error:       e.Error,
		Message:     message(e.Message),
		Call:        toolCall(e.Call, e.Rewritten),
		Rewritten:   e.Rewritten,
		IsError:     e.IsError,
		ApprovalSeq: e.ApprovalSeq,
		Approval:    approvalStatuses[e.Approval],
		DecidedBy:   e.DecidedBy,
	}
}

func Submission(s durable.Submission) *durablev1.Submission {
	return &durablev1.Submission{
		Seq:       s.Seq,
		RequestId: s.RequestID,
		Text:      s.Text,
		Status:    submissionStatuses[s.Status],
		Result:    s.Result,
		Error:     s.Error,
	}
}

func Approval(a durable.Approval) *durablev1.Approval {
	return &durablev1.Approval{
		Seq:        a.Seq,
		Submission: a.Submission,
		Call:       toolCall(&a.Call, a.Rewritten),
		Rewritten:  a.Rewritten,
		Status:     approvalStatuses[a.Status],
		DecidedBy:  a.DecidedBy,
	}
}

func toolCall(c *durable.ToolCall, redact bool) *durablev1.ToolCall {
	if c == nil {
		return nil
	}
	out := &durablev1.ToolCall{Id: c.ID, Name: c.Name}
	if !redact {
		out.Input = c.Input
	}
	return out
}

// message omits Raw, which is provider-specific and replayed server side only.
func message(m *durable.Message) *durablev1.Message {
	if m == nil {
		return nil
	}
	out := &durablev1.Message{
		Role:          roles[m.Role],
		Text:          m.Text,
		ToolCallId:    m.ToolCallID,
		IsError:       m.IsError,
		ContextTokens: m.ContextTokens,
	}
	for i := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, toolCall(&m.ToolCalls[i], false))
	}
	return out
}

var deltaKinds = map[durable.DeltaKind]durablev1.DeltaKind{
	durable.DeltaReset:      durablev1.DeltaKind_DELTA_KIND_RESET,
	durable.DeltaText:       durablev1.DeltaKind_DELTA_KIND_TEXT,
	durable.DeltaToolOutput: durablev1.DeltaKind_DELTA_KIND_TOOL_OUTPUT,
}

// Delta converts d; lastSeq is the newest committed seq when it was produced.
func Delta(d durable.Delta, lastSeq int64) *durablev1.Delta {
	return &durablev1.Delta{
		LastSeq:    lastSeq,
		Kind:       deltaKinds[d.Kind],
		Submission: d.Submission,
		CallId:     d.CallID,
		Text:       d.Text,
	}
}
