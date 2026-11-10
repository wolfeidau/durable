package openshell

import (
	"testing"

	"github.com/ai-openshell/sdk-go/openshell/v1/fake"
	"github.com/ai-openshell/sdk-go/openshell/v1/types"
	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/sandbox"
)

func TestEnsureCreatesMissingAndStartsStopped(t *testing.T) {
	fc := fake.NewClient()
	l := newWithClient(fc, "")
	spec := sandbox.Spec{Name: "dt-a", Image: "img", Token: "t"}

	require.NoError(t, l.Ensure(t.Context(), spec))
	sb, err := fc.Sandboxes().Get(t.Context(), "default", "dt-a")
	require.NoError(t, err)
	require.Equal(t, "img", sb.Spec.Template.Image)
	require.Equal(t, types.SandboxReady, sb.Status.Phase)

	require.NoError(t, l.Stop(t.Context(), "dt-a"))
	phase, err := l.Phase(t.Context(), "dt-a")
	require.NoError(t, err)
	require.Equal(t, types.SandboxStopped, phase)

	require.NoError(t, l.Ensure(t.Context(), spec))
	phase, err = l.Phase(t.Context(), "dt-a")
	require.NoError(t, err)
	require.Equal(t, types.SandboxReady, phase)
}

func seedError(fc *fake.Client, reason string) {
	fc.AddSandbox("default", &types.Sandbox{Name: "dt-a", Workspace: "default", Status: types.SandboxStatus{
		Phase:      types.SandboxError,
		Conditions: []types.SandboxCondition{{Type: "Ready", Status: "False", Reason: reason}},
	}})
}

func TestStartRecoversStartFailedWithStopThenStart(t *testing.T) {
	fc := fake.NewClient()
	seedError(fc, "StartFailed")
	l := newWithClient(fc, "")

	require.NoError(t, l.Start(t.Context(), "dt-a"))
	phase, err := l.Phase(t.Context(), "dt-a")
	require.NoError(t, err)
	require.Equal(t, types.SandboxReady, phase)
}

func TestStartOtherErrorReasonStaysAnError(t *testing.T) {
	fc := fake.NewClient()
	seedError(fc, "MainProcessFailed")
	l := newWithClient(fc, "")

	require.ErrorContains(t, l.Start(t.Context(), "dt-a"), "phase Error")
	phase, err := l.Phase(t.Context(), "dt-a")
	require.NoError(t, err)
	require.Equal(t, types.SandboxError, phase)
}
