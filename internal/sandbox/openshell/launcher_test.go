package openshell

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ai-openshell/sdk-go/openshell/v1/fake"
	"github.com/ai-openshell/sdk-go/openshell/v1/types"
	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/worker"
)

func seed(fc *fake.Client, name string, phase types.SandboxPhase) {
	fc.AddSandbox("default", &types.Sandbox{Name: name, Workspace: "default", Status: types.SandboxStatus{Phase: phase}})
}

func TestCreatePassesTokenInEnvironmentOnly(t *testing.T) {
	fc := fake.NewClient()
	l := newWithClient(fc, "")

	require.NoError(t, l.Create(t.Context(), Spec{Name: "dt-a", Image: "img:1", Token: "s3cret"}))

	sb, err := fc.Sandboxes().Get(t.Context(), "default", "dt-a")
	require.NoError(t, err)
	require.Equal(t, types.SandboxReady, sb.Status.Phase)
	require.Equal(t, "img:1", sb.Spec.Template.Image)
	require.Equal(t, []string{"/usr/local/bin/durable-worker", "--addr", "127.0.0.1:8080", "--root", "/sandbox"}, sb.Spec.Command)
	require.Equal(t, map[string]string{worker.TokenEnv: "s3cret"}, sb.Spec.Environment)
	require.NotContains(t, fmt.Sprint(sb.Spec.Command), "s3cret")
}

func TestCreateUsesConfiguredWorkspace(t *testing.T) {
	fc := fake.NewClient()
	require.NoError(t, newWithClient(fc, "team").Create(t.Context(), Spec{Name: "dt-a", Image: "img", Token: "t"}))

	_, err := fc.Sandboxes().Get(t.Context(), "team", "dt-a")
	require.NoError(t, err)
}

func TestStartByPhase(t *testing.T) {
	tests := []struct {
		phase   types.SandboxPhase
		wantErr bool
	}{
		{phase: types.SandboxReady},
		{phase: types.SandboxStopped},
		{phase: types.SandboxStopping},
		{phase: types.SandboxProvisioning},
		{phase: types.SandboxStarting},
		{phase: types.SandboxError, wantErr: true},
		{phase: types.SandboxDeleting, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(string(tc.phase), func(t *testing.T) {
			fc := fake.NewClient()
			seed(fc, "dt-a", tc.phase)
			l := newWithClient(fc, "")

			err := l.Start(t.Context(), "dt-a")
			phase, perr := l.Phase(t.Context(), "dt-a")
			require.NoError(t, perr)
			if tc.wantErr {
				require.Error(t, err)
				require.Equal(t, tc.phase, phase)
				return
			}
			require.NoError(t, err)
			require.Equal(t, types.SandboxReady, phase)
		})
	}
}

func TestStartMissingIsNotFound(t *testing.T) {
	err := newWithClient(fake.NewClient(), "").Start(t.Context(), "dt-none")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestStopStopsAndMissingIsNoOp(t *testing.T) {
	fc := fake.NewClient()
	seed(fc, "dt-a", types.SandboxReady)
	l := newWithClient(fc, "")

	require.NoError(t, l.Stop(t.Context(), "dt-a"))
	phase, err := l.Phase(t.Context(), "dt-a")
	require.NoError(t, err)
	require.Equal(t, types.SandboxStopped, phase)

	require.NoError(t, l.Stop(t.Context(), "dt-none"))
}

func TestDeleteMissingIsNoOp(t *testing.T) {
	fc := fake.NewClient()
	seed(fc, "dt-a", types.SandboxReady)
	l := newWithClient(fc, "")

	require.NoError(t, l.Delete(t.Context(), "dt-a"))
	require.NoError(t, l.Delete(t.Context(), "dt-a"))
	_, err := l.Phase(t.Context(), "dt-a")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPhaseMissingIsErrNotFound(t *testing.T) {
	_, err := newWithClient(fake.NewClient(), "").Phase(t.Context(), "dt-none")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestClosedGatewayIsUnavailable(t *testing.T) {
	fc := fake.NewClient()
	l := newWithClient(fc, "")
	require.NoError(t, l.Close())

	err := l.Create(t.Context(), Spec{Name: "dt-a", Image: "img", Token: "t"})
	require.Error(t, err)
	require.True(t, IsUnavailable(err))
}

func TestIsUnavailable(t *testing.T) {
	require.True(t, IsUnavailable(&types.StatusError{Code: types.ErrorUnavailable}))
	require.True(t, IsUnavailable(&types.StatusError{Code: types.ErrorDeadlineExceeded}))
	require.True(t, IsUnavailable(fmt.Errorf("wrapped: %w", context.DeadlineExceeded)))
	require.True(t, IsUnavailable(fmt.Errorf("wrapped: %w", errWorkerUnreachable)))

	require.False(t, IsUnavailable(nil))
	require.False(t, IsUnavailable(errors.New("boom")))
	require.False(t, IsUnavailable(context.Canceled))
	require.False(t, IsUnavailable(ErrNotFound))
	require.False(t, IsUnavailable(&types.StatusError{Code: types.ErrorInvalidArgument}))
	require.False(t, IsUnavailable(&types.StatusError{Code: types.ErrorNotFound}))
}
