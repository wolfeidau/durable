package openshell

import (
	"context"
	"errors"
	"fmt"
	"testing"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"
	"github.com/ai-openshell/sdk-go/openshell/v1/fake"
	"github.com/ai-openshell/sdk-go/openshell/v1/types"
	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/worker"
)

func seed(fc *fake.Client, phase types.SandboxPhase) {
	fc.AddSandbox("default", &types.Sandbox{Name: "dt-a", Workspace: "default", Status: types.SandboxStatus{Phase: phase}})
}

func TestCreatePassesTokenInEnvironmentOnly(t *testing.T) {
	fc := fake.NewClient()
	l := newWithClient(fc, "", nil)

	require.NoError(t, l.Create(t.Context(), sandbox.Spec{Name: "dt-a", Image: "img:1", Token: "s3cret"}))

	sb, err := fc.Sandboxes().Get(t.Context(), "default", "dt-a")
	require.NoError(t, err)
	require.Equal(t, types.SandboxReady, sb.Status.Phase)
	require.Equal(t, "img:1", sb.Spec.Template.Image)
	require.Equal(t, []string{"/usr/local/bin/durable-worker", "--addr", "127.0.0.1:8080", "--root", "/sandbox"}, sb.Spec.Command)
	require.Equal(t, map[string]string{worker.TokenEnv: "s3cret"}, sb.Spec.Environment)
	require.NotContains(t, fmt.Sprint(sb.Spec.Command), "s3cret")
	require.Nil(t, sb.Spec.Policy, "no egress means no policy, which leaves the sandbox without egress")
}

func TestCreateAllowsEgressToTheWorkerBinaryOnly(t *testing.T) {
	fc := fake.NewClient()
	egress := []sandbox.Endpoint{{Host: "mcp.example.com", Port: 443}, {Host: "plain.example.com", Port: 80}}

	require.NoError(t, newWithClient(fc, "", nil).Create(t.Context(), sandbox.Spec{Name: "dt-a", Image: "img", Token: "t", Egress: egress}))

	sb, err := fc.Sandboxes().Get(t.Context(), "default", "dt-a")
	require.NoError(t, err)
	p := sb.Spec.Policy
	require.NotNil(t, p)
	// Dropping Landlock would leave the sandbox unconfined, and the filesystem and process defaults must stay with the gateway.
	require.Equal(t, &types.LandlockPolicy{Compatibility: "best_effort"}, p.Landlock)
	require.Nil(t, p.Filesystem)
	require.Nil(t, p.Process)
	require.Equal(t, map[string]types.NetworkPolicyRule{"durable-mcp": {
		Name: "durable-mcp",
		Endpoints: []types.PolicyNetworkEndpoint{
			{Host: "mcp.example.com", Port: 443},
			{Host: "plain.example.com", Port: 80},
		},
		Binaries: []types.PolicyNetworkBinary{{Path: "/usr/local/bin/durable-worker"}},
	}}, p.NetworkPolicies)
}

func TestCreateUsesConfiguredWorkspace(t *testing.T) {
	fc := fake.NewClient()
	require.NoError(t, newWithClient(fc, "team", nil).Create(t.Context(), sandbox.Spec{Name: "dt-a", Image: "img", Token: "t"}))

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
		{phase: types.SandboxCompleted, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(string(tc.phase), func(t *testing.T) {
			fc := fake.NewClient()
			seed(fc, tc.phase)
			l := newWithClient(fc, "", nil)

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
	err := newWithClient(fake.NewClient(), "", nil).Start(t.Context(), "dt-none")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestStopStopsAndMissingIsNoOp(t *testing.T) {
	fc := fake.NewClient()
	seed(fc, types.SandboxReady)
	l := newWithClient(fc, "", nil)

	require.NoError(t, l.Stop(t.Context(), "dt-a"))
	phase, err := l.Phase(t.Context(), "dt-a")
	require.NoError(t, err)
	require.Equal(t, types.SandboxStopped, phase)

	require.NoError(t, l.Stop(t.Context(), "dt-none"))
}

func TestDeleteMissingIsNoOp(t *testing.T) {
	fc := fake.NewClient()
	seed(fc, types.SandboxReady)
	l := newWithClient(fc, "", nil)

	require.NoError(t, l.Delete(t.Context(), "dt-a"))
	require.NoError(t, l.Delete(t.Context(), "dt-a"))
	_, err := l.Phase(t.Context(), "dt-a")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPhaseMissingIsErrNotFound(t *testing.T) {
	_, err := newWithClient(fake.NewClient(), "", nil).Phase(t.Context(), "dt-none")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestClosedGatewayIsUnavailable(t *testing.T) {
	fc := fake.NewClient()
	l := newWithClient(fc, "", nil)
	require.NoError(t, l.Close())

	err := l.Create(t.Context(), sandbox.Spec{Name: "dt-a", Image: "img", Token: "t"})
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

// waitClient makes WaitReady report phase, or fail with err, instead of the fake's instant Ready.
type waitClient struct {
	*fake.Client
	phase types.SandboxPhase
	err   error
}

func (c waitClient) Sandboxes() v1.SandboxInterface {
	return waitSandboxes{SandboxInterface: c.Client.Sandboxes(), c: c}
}

type waitSandboxes struct {
	v1.SandboxInterface
	c waitClient
}

func (s waitSandboxes) WaitReady(ctx context.Context, ws, name string, _ ...v1.WaitOptions) (*types.Sandbox, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	sb, err := s.Get(ctx, ws, name)
	if err != nil {
		return nil, err
	}
	sb.Status.Phase = s.c.phase
	return sb, nil
}

func TestCreateRequiresNameImageAndToken(t *testing.T) {
	l := newWithClient(fake.NewClient(), "", nil)
	for _, s := range []sandbox.Spec{
		{Image: "img", Token: "t"},
		{Name: "dt-a", Token: "t"},
		{Name: "dt-a", Image: "img"},
	} {
		require.Error(t, l.Create(t.Context(), s))
	}
}

func TestCreateNeverRestartsTheWorker(t *testing.T) {
	fc := fake.NewClient()
	require.NoError(t, newWithClient(fc, "", nil).Create(t.Context(), sandbox.Spec{Name: "dt-a", Image: "img", Token: "t"}))

	sb, err := fc.Sandboxes().Get(t.Context(), "default", "dt-a")
	require.NoError(t, err)
	require.Equal(t, types.SandboxRestartNever, sb.Spec.RestartPolicy)
}

func TestCreateWithExitedWorkerFailsAndDeletes(t *testing.T) {
	c := waitClient{Client: fake.NewClient(), phase: types.SandboxCompleted}
	l := newWithClient(c, "", nil)

	err := l.Create(t.Context(), sandbox.Spec{Name: "dt-a", Image: "img", Token: "t"})
	require.ErrorContains(t, err, "worker exited")
	require.False(t, IsUnavailable(err))
	_, err = l.Phase(t.Context(), "dt-a")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestCreateThatNeverGetsReadyDeletes(t *testing.T) {
	c := waitClient{Client: fake.NewClient(), err: &types.StatusError{Code: types.ErrorDeadlineExceeded, Message: "slow"}}
	l := newWithClient(c, "", nil)

	err := l.Create(t.Context(), sandbox.Spec{Name: "dt-a", Image: "img", Token: "t"})
	require.True(t, IsUnavailable(err))
	_, err = l.Phase(t.Context(), "dt-a")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestStartWithExitedWorkerFails(t *testing.T) {
	c := waitClient{Client: fake.NewClient(), phase: types.SandboxCompleted}
	seed(c.Client, types.SandboxStarting)

	err := newWithClient(c, "", nil).Start(t.Context(), "dt-a")
	require.ErrorContains(t, err, "worker exited")
	require.False(t, IsUnavailable(err))
}
