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
	_, _, oldRelease, err := sandbox.Open(t.Context(), b, pin, false)
	require.NoError(t, err)
	_, _, newRelease, err := sandbox.Open(t.Context(), b, pin, false)
	require.NoError(t, err)

	require.NoError(t, oldRelease(false))
	_, stops, _ := f.Counts()
	require.Zero(t, stops)
	require.Equal(t, sandboxtest.Ready, f.Phase(pin.Name))

	require.NoError(t, newRelease(false))
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
	_, _, _, err = sandbox.Open(t.Context(), b, pin, false)
	require.Error(t, err)
	f.FailEnsure(nil)

	_, _, release, err := sandbox.Open(t.Context(), b, pin, false)
	require.NoError(t, err)
	require.NoError(t, release(false))
	_, stops, _ := f.Counts()
	require.Equal(t, 1, stops, "the only user's release stops the sandbox")
}

func TestSharedDeletesOnlyAfterTheLastUser(t *testing.T) {
	f := sandboxtest.New(t)
	b := sandbox.Shared(f)
	pin, err := sandbox.WritePin(t.TempDir(), "img:1")
	require.NoError(t, err)

	_, _, other, err := sandbox.Open(t.Context(), b, pin, false)
	require.NoError(t, err)
	_, _, release, err := sandbox.Open(t.Context(), b, pin, true)
	require.NoError(t, err)

	// A discard while another session holds the sandbox must not delete it under that session.
	require.NoError(t, release(true))
	_, _, deletes := f.Counts()
	require.Zero(t, deletes)
	require.Equal(t, sandboxtest.Ready, f.Phase(pin.Name))

	require.NoError(t, other(true))
	_, _, deletes = f.Counts()
	require.Equal(t, 1, deletes)
	require.Empty(t, f.Phase(pin.Name))
}
