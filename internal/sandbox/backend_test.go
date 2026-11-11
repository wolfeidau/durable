package sandbox_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/sandbox/sandboxtest"
	"github.com/wolfeidau/durable/tools"
)

func TestWritePinWritesOnceWithMode0600(t *testing.T) {
	dir := t.TempDir()

	first, err := sandbox.WritePin(dir, "img:1")
	require.NoError(t, err)
	require.Regexp(t, `^ds-[0-9a-f]{16}$`, first.Name)
	require.Equal(t, "img:1", first.Image)
	require.NotEmpty(t, first.Token)

	st, err := os.Stat(filepath.Join(dir, sandbox.PinFile))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())

	second, err := sandbox.WritePin(dir, "img:2")
	require.NoError(t, err)
	require.Equal(t, first, second)

	read, ok, err := sandbox.ReadPin(dir)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, first, read)
}

func TestPinRoundTripsGitAccessAndOmitsItWhenEmpty(t *testing.T) {
	access := []sandbox.GitAccess{{
		Kind: "github", Provider: "durable-github", ProfileID: "durable-github", CredentialEnv: "GITHUB_TOKEN",
		Endpoint: sandbox.Endpoint{Host: "github.com", Port: 443},
		Rules:    []sandbox.HTTPRule{{Method: "GET", Path: "/o/n.git/info/refs*"}},
		Binaries: []string{"/usr/bin/git"},
	}}
	dir := t.TempDir()
	pin, err := sandbox.WritePinGit(dir, "img", nil, access)
	require.NoError(t, err)
	require.Equal(t, access, pin.Git)
	require.Equal(t, access, pin.Spec().Git)
	read, ok, err := sandbox.ReadPin(dir)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, access, read.Git)

	plain := t.TempDir()
	_, err = sandbox.WritePin(plain, "img")
	require.NoError(t, err)
	b, err := os.ReadFile(filepath.Join(plain, sandbox.PinFile))
	require.NoError(t, err)
	require.NotContains(t, string(b), `"git"`)
}

func TestWritePinNamesDiffer(t *testing.T) {
	a, err := sandbox.WritePin(t.TempDir(), "img")
	require.NoError(t, err)
	b, err := sandbox.WritePin(t.TempDir(), "img")
	require.NoError(t, err)
	require.NotEqual(t, a.Name, b.Name)
	require.NotEqual(t, a.Token, b.Token)
}

func TestReadPinMissingAndInvalid(t *testing.T) {
	dir := t.TempDir()
	_, ok, err := sandbox.ReadPin(dir)
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, os.WriteFile(filepath.Join(dir, sandbox.PinFile), []byte(`{"name":"ds-x"}`), 0o600))
	_, _, err = sandbox.ReadPin(dir)
	require.ErrorContains(t, err, "incomplete")
}

func TestOpenReturnsCatalogToolsAndReleaseStops(t *testing.T) {
	f := sandboxtest.New(t)
	pin := sandbox.Pin{Name: "ds-a", Image: "img", Token: "tok"}

	got, _, release, err := sandbox.Open(t.Context(), f, pin, false)
	require.NoError(t, err)

	var names, want []string
	for _, tl := range got {
		names = append(names, tl.Spec().Name)
	}
	for _, e := range tools.Catalog() {
		want = append(want, e.Spec.Name)
	}
	require.Equal(t, want, names)
	require.Equal(t, sandboxtest.Ready, f.Phase("ds-a"))
	require.Equal(t, []sandbox.Spec{pin.Spec()}, f.Specs())

	require.NoError(t, release(false))
	require.NoError(t, release(false))
	_, stops, _ := f.Counts()
	require.Equal(t, 1, stops)
	require.Equal(t, sandboxtest.Stopped, f.Phase("ds-a"))
}

func TestOpenWrapsErrors(t *testing.T) {
	boom := errors.New("bad image")
	tests := []struct {
		name        string
		fail        func(*sandboxtest.Fake, error)
		err         error
		unavailable bool
		stops       int
	}{
		{name: "ensure configuration", fail: (*sandboxtest.Fake).FailEnsure, err: boom},
		{name: "ensure unavailable", fail: (*sandboxtest.Fake).FailEnsure, err: fmt.Errorf("gateway: %w", sandboxtest.ErrUnavailable), unavailable: true},
		{name: "connect configuration", fail: (*sandboxtest.Fake).FailConnect, err: boom, stops: 1},
		{name: "connect unavailable", fail: (*sandboxtest.Fake).FailConnect, err: sandboxtest.ErrUnavailable, unavailable: true, stops: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := sandboxtest.New(t)
			tc.fail(f, tc.err)

			_, _, _, err := sandbox.Open(t.Context(), f, sandbox.Pin{Name: "ds-a", Image: "img", Token: "tok"}, false)

			var se *sandbox.Error
			require.ErrorAs(t, err, &se)
			require.Equal(t, tc.unavailable, se.Unavailable)
			require.ErrorIs(t, err, tc.err)
			_, stops, _ := f.Counts()
			require.Equal(t, tc.stops, stops)
		})
	}
}

func TestOpenFailureDeletesAFreshSandboxAndStopsAnExistingOne(t *testing.T) {
	boom := errors.New("catalog mismatch")
	tests := []struct {
		name           string
		fresh          bool
		stops, deletes int
		phase          string
	}{
		{name: "fresh", fresh: true, deletes: 1},
		{name: "existing", stops: 1, phase: sandboxtest.Stopped},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := sandboxtest.New(t)
			f.FailConnect(boom)

			_, _, _, err := sandbox.Open(t.Context(), f, sandbox.Pin{Name: "ds-a", Image: "img", Token: "tok"}, tc.fresh)

			require.ErrorIs(t, err, boom)
			_, stops, deletes := f.Counts()
			require.Equal(t, tc.stops, stops)
			require.Equal(t, tc.deletes, deletes)
			require.Equal(t, tc.phase, f.Phase("ds-a"))
		})
	}
}
