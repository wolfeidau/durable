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
func fakeOp(t *testing.T, script string) { t.Helper(); fakeBin(t, "op", script) }

func fakeBin(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700))
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
	require.ErrorContains(t, err, "exit status 1")
	require.NotContains(t, err.Error(), "not signed in", "stderr can echo a reference")
}

func TestResolveSources(t *testing.T) {
	src := func(value string) Source { return Source{Flag: "--tok", Env: "DURABLE_TEST_TOK", Value: value} }
	ctx := context.Background()

	t.Run("op", func(t *testing.T) {
		fakeOp(t, `[ "$1 $2 $3" = "read -n `+ref+`" ] && printf 'op-secret\n'`)
		got, err := Resolve(ctx, src(ref))
		require.NoError(t, err)
		require.Equal(t, "op-secret", got)
	})
	t.Run("gh", func(t *testing.T) {
		fakeBin(t, "gh", `[ "$1 $2" = "auth token" ] && printf 'gh-secret\n'`)
		got, err := Resolve(ctx, src("gh"))
		require.NoError(t, err)
		require.Equal(t, "gh-secret", got)
	})
	t.Run("env", func(t *testing.T) {
		t.Setenv("DURABLE_TEST_TOK", " env-secret\n")
		got, err := Resolve(ctx, src(""))
		require.NoError(t, err)
		require.Equal(t, "env-secret", got)
	})
	t.Run("env reference", func(t *testing.T) {
		t.Setenv("DURABLE_TEST_REF", " ref-secret\n")
		got, err := Resolve(ctx, src("env:DURABLE_TEST_REF"))
		require.NoError(t, err)
		require.Equal(t, "ref-secret", got)
	})
	t.Run("unset env reference names the variable only", func(t *testing.T) {
		t.Setenv("DURABLE_TEST_REF", "")
		_, err := Resolve(ctx, src("env:DURABLE_TEST_REF"))
		require.ErrorContains(t, err, "--tok")
		require.ErrorContains(t, err, "DURABLE_TEST_REF")
	})
	t.Run("nothing set", func(t *testing.T) {
		t.Setenv("DURABLE_TEST_TOK", "")
		_, err := Resolve(ctx, src(""))
		require.ErrorContains(t, err, "--tok")
		require.ErrorContains(t, err, "DURABLE_TEST_TOK")
	})
	t.Run("empty secret", func(t *testing.T) {
		fakeBin(t, "gh", `exit 0`)
		_, err := Resolve(ctx, src("gh"))
		require.ErrorContains(t, err, "empty secret")
	})
	t.Run("failing binary", func(t *testing.T) {
		fakeBin(t, "gh", `echo "ghp_leaked" >&2; exit 3`)
		_, err := Resolve(ctx, src("gh"))
		require.ErrorContains(t, err, "--tok")
		require.ErrorContains(t, err, "exit status 3")
		require.NotContains(t, err.Error(), "ghp_leaked")
	})
	t.Run("failing op hides the reference", func(t *testing.T) {
		fakeOp(t, `echo "cannot read $3" >&2; exit 1`)
		_, err := Resolve(ctx, src(ref))
		require.Error(t, err)
		require.NotContains(t, err.Error(), "cannot read")
		require.NotContains(t, err.Error(), ref)
	})
	t.Run("literal value is rejected unechoed", func(t *testing.T) {
		_, err := Resolve(ctx, src("ghp_pasted"))
		require.ErrorContains(t, err, "--tok")
		require.NotContains(t, err.Error(), "ghp_pasted")
	})
}

func TestResolveRejectsEmptySecret(t *testing.T) {
	fakeOp(t, `exit 0`)
	_, err := ResolveAPIToken(context.Background(), "", ref)
	require.ErrorContains(t, err, "empty secret")
}
