package wire

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	durablev1 "github.com/wolfeidau/durable/api/durable/v1"
)

var secret = json.RawMessage(`{"command":"echo <&>","token":"s3cret"}`)

func TestEventRewrittenOmitsInput(t *testing.T) {
	for _, kind := range []durable.EventKind{
		durable.EventCallStarted, durable.EventCallDone, durable.EventApprovalRequested,
	} {
		got := Event(durable.Event{
			Kind:      kind,
			Call:      &durable.ToolCall{ID: "c1", Name: "bash", Input: secret},
			Rewritten: true,
		}, "ep")
		require.NotNil(t, got.Call, kind)
		require.Equal(t, "c1", got.Call.Id)
		require.Equal(t, "bash", got.Call.Name)
		require.Empty(t, got.Call.Input, kind)
		require.True(t, got.Rewritten)
	}
}

func TestEventKeepsInputWhenNotRewritten(t *testing.T) {
	got := Event(durable.Event{
		Kind: durable.EventCallStarted,
		Call: &durable.ToolCall{ID: "c1", Name: "bash", Input: secret},
	}, "ep")
	require.Equal(t, []byte(secret), got.Call.Input)
	require.Equal(t, "ep", got.Epoch)
}

func TestApprovalRewrittenOmitsInput(t *testing.T) {
	call := durable.ToolCall{ID: "c1", Name: "bash", Input: secret}
	require.Empty(t, Approval(durable.Approval{Call: call, Rewritten: true}).Call.Input)
	require.Equal(t, []byte(secret), Approval(durable.Approval{Call: call}).Call.Input)
}

func TestEventOmitsRaw(t *testing.T) {
	got := Event(durable.Event{
		Kind: durable.EventMessage,
		Message: &durable.Message{
			Role: durable.RoleAssistant, Raw: json.RawMessage(`{"x":1}`),
			ToolCalls: []durable.ToolCall{{ID: "c1", Name: "bash", Input: secret}},
		},
	}, "ep")
	require.Equal(t, durablev1.Role_ROLE_ASSISTANT, got.Message.Role)
	require.Equal(t, []byte(secret), got.Message.ToolCalls[0].Input)
}

func TestNilMessageAndCall(t *testing.T) {
	require.NotPanics(t, func() {
		got := Event(durable.Event{Kind: durable.EventMessage}, "")
		require.Nil(t, got.Message)
		require.Nil(t, got.Call)
		Event(durable.Event{Kind: durable.EventCallDone, Rewritten: true}, "")
	})
}

func TestEventKindsMapped(t *testing.T) {
	kinds := []durable.EventKind{
		durable.EventSubmission, durable.EventMessage, durable.EventCallStarted,
		durable.EventCallReplayed, durable.EventCallDone, durable.EventCompacted,
		durable.EventCompactionFailed, durable.EventApprovalRequested, durable.EventApprovalDecided,
	}
	seen := map[durablev1.EventKind]bool{}
	for _, k := range kinds {
		got := Event(durable.Event{Kind: k}, "").Kind
		require.NotEqual(t, durablev1.EventKind_EVENT_KIND_UNSPECIFIED, got, k)
		seen[got] = true
	}
	// A kind added to the schema but not to this table fails here.
	require.Len(t, seen, len(durablev1.EventKind_name)-1)
}

func TestSubmissionStatusesMapped(t *testing.T) {
	all := []durable.SubmissionStatus{
		durable.StatusQueued, durable.StatusRunning, durable.StatusDone,
		durable.StatusFailed, durable.StatusCancelled,
	}
	seen := map[durablev1.SubmissionStatus]bool{}
	for _, s := range all {
		got := Submission(durable.Submission{Status: s}).Status
		require.NotEqual(t, durablev1.SubmissionStatus_SUBMISSION_STATUS_UNSPECIFIED, got, s)
		seen[got] = true
	}
	require.Len(t, seen, len(durablev1.SubmissionStatus_name)-1)
}

func TestApprovalStatusesMapped(t *testing.T) {
	all := []durable.ApprovalStatus{
		durable.ApprovalPending, durable.ApprovalAllowed,
		durable.ApprovalDeclined, durable.ApprovalCancelled,
	}
	seen := map[durablev1.ApprovalStatus]bool{}
	for _, s := range all {
		got := Approval(durable.Approval{Status: s}).Status
		require.NotEqual(t, durablev1.ApprovalStatus_APPROVAL_STATUS_UNSPECIFIED, got, s)
		seen[got] = true
	}
	require.Len(t, seen, len(durablev1.ApprovalStatus_name)-1)
}
