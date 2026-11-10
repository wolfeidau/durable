package openshell

import (
	"context"
	"testing"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"
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

// lagClient reports Error from Get for a few calls after Stop, as a gateway may before the stop takes effect.
type lagClient struct {
	*fake.Client
	lag *int
}

func (c lagClient) Sandboxes() v1.SandboxInterface {
	return lagSandboxes{SandboxInterface: c.Client.Sandboxes(), lag: c.lag}
}

type lagSandboxes struct {
	v1.SandboxInterface
	lag *int
}

func (s lagSandboxes) Stop(ctx context.Context, ws, name string) (*types.Sandbox, error) {
	*s.lag = 2
	return s.SandboxInterface.Stop(ctx, ws, name)
}

func (s lagSandboxes) Get(ctx context.Context, ws, name string) (*types.Sandbox, error) {
	sb, err := s.SandboxInterface.Get(ctx, ws, name)
	if err == nil && *s.lag > 0 {
		*s.lag--
		sb.Status.Phase = types.SandboxError
	}
	return sb, err
}

func TestStartFailedRecoveryWaitsThroughALaggingError(t *testing.T) {
	fc := fake.NewClient()
	seedError(fc, "StartFailed")
	lag := 0
	l := newWithClient(lagClient{Client: fc, lag: &lag}, "")

	require.NoError(t, l.Start(t.Context(), "dt-a"))
	require.Zero(t, lag, "the recovery polled through the lagging Error")
}
