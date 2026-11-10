package sandbox_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/sandbox/sandboxtest"
)

func TestSharedStopsOnlyAfterTheLastRelease(t *testing.T) {
	f := sandboxtest.New(t)
	b := sandbox.Shared(f)
	pin, err := sandbox.WritePin(t.TempDir(), "img:1")
	require.NoError(t, err)

	// An idle close that releases after a reopen has ensured the sandbox must not stop it under the new session.
	_, oldRelease, err := sandbox.Open(t.Context(), b, pin)
	require.NoError(t, err)
	_, newRelease, err := sandbox.Open(t.Context(), b, pin)
	require.NoError(t, err)

	require.NoError(t, oldRelease())
	_, stops, _ := f.Counts()
	require.Zero(t, stops)
	require.Equal(t, sandboxtest.Ready, f.Phase(pin.Name))

	require.NoError(t, newRelease())
	_, stops, _ = f.Counts()
	require.Equal(t, 1, stops)
	require.Equal(t, sandboxtest.Stopped, f.Phase(pin.Name))
}

func TestSharedFailedEnsureIsNotAUser(t *testing.T) {
	f := sandboxtest.New(t)
	b := sandbox.Shared(f)
	pin, err := sandbox.WritePin(t.TempDir(), "img:1")
	require.NoError(t, err)

	f.FailEnsure(errors.New("boom"))
	_, _, err = sandbox.Open(t.Context(), b, pin)
	require.Error(t, err)
	f.FailEnsure(nil)

	_, release, err := sandbox.Open(t.Context(), b, pin)
	require.NoError(t, err)
	require.NoError(t, release())
	_, stops, _ := f.Counts()
	require.Equal(t, 1, stops, "the only user's release stops the sandbox")
}
