package creds

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const ref = "op://vault/item/field"

// fakeOp puts an op script first on PATH.
func fakeOp(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "op")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700)) //nolint:gosec // the fake must be executable
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestResolveNoFlagsUsesDefaultCredentials(t *testing.T) {
	token, err := ResolveAPIToken(context.Background(), "", "")
	require.NoError(t, err)
	require.Empty(t, token)
}

func TestResolveRejectsBothFlags(t *testing.T) {
	_, err := ResolveAPIToken(context.Background(), "a", ref)
	require.ErrorContains(t, err, "cannot specify both")
}

func TestResolveDirectToken(t *testing.T) {
	token, err := ResolveAPIToken(context.Background(), "direct", "")
	require.NoError(t, err)
	require.Equal(t, "direct", token)
}

func TestResolveFrom1Password(t *testing.T) {
	fakeOp(t, `[ "$1 $2 $3" = "read -n `+ref+`" ] && printf 'secret\n'`)
	token, err := ResolveAPIToken(context.Background(), "", ref)
	require.NoError(t, err)
	require.Equal(t, "secret", token)
}

func TestResolveRejectsNonReferenceWithoutEchoingIt(t *testing.T) {
	fakeOp(t, `echo "op must not run: $3" >&2; exit 1`)
	_, err := ResolveAPIToken(context.Background(), "", "sk-pasted-token")
	require.ErrorContains(t, err, "op:// secret reference")
	require.NotContains(t, err.Error(), "sk-pasted-token")
}

func TestResolveReportsOpFailure(t *testing.T) {
	fakeOp(t, `echo "not signed in" >&2; exit 1`)
	_, err := ResolveAPIToken(context.Background(), "", ref)
	require.ErrorContains(t, err, "not signed in")
}

func TestResolveRejectsEmptySecret(t *testing.T) {
	fakeOp(t, `exit 0`)
	_, err := ResolveAPIToken(context.Background(), "", ref)
	require.ErrorContains(t, err, "empty secret")
}
