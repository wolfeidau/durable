// Package creds resolves secrets from command line flags and the environment.
package creds

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	opRefPrefix = "op://"
	ghSource    = "gh"
)

// Source says where a secret comes from.
type Source struct {
	// Flag names the option that sets Value, for errors.
	Flag string
	// Env is read when Value is empty.
	Env string
	// Value is an op:// reference, the literal "gh" for the GitHub CLI's token, or empty.
	Value string
}

// Resolve returns the secret. Errors name the flag and variable, never the value or a command's stderr, because either
// can echo a reference or a secret.
func Resolve(ctx context.Context, s Source) (string, error) {
	switch {
	case s.Value == ghSource:
		secret, err := run(ctx, "gh", "auth", "token")
		if err != nil {
			return "", fmt.Errorf("failed to read %s from gh: %w", s.Flag, err)
		}
		return secret, nil
	case strings.HasPrefix(s.Value, opRefPrefix):
		secret, err := readOp(ctx, s.Value)
		if err != nil {
			return "", fmt.Errorf("failed to read %s from 1Password: %w", s.Flag, err)
		}
		return secret, nil
	case s.Value != "":
		return "", fmt.Errorf("%s must be an op:// secret reference or gh", s.Flag)
	}
	secret := strings.TrimSpace(os.Getenv(s.Env))
	if secret == "" {
		return "", fmt.Errorf("no token: set %s or %s", s.Flag, s.Env)
	}
	return secret, nil
}

// ResolveAPIToken returns the token chosen by flags, or "" so the SDK's default credentials apply.
func ResolveAPIToken(ctx context.Context, token, tokenFrom1Password string) (string, error) {
	if token != "" && tokenFrom1Password != "" {
		return "", errors.New("cannot specify both --api-token and --api-token-from-1password")
	}
	if tokenFrom1Password == "" {
		return token, nil
	}

	// Never echo the value: a token pasted here by mistake must not reach an error or op's stderr.
	if !strings.HasPrefix(tokenFrom1Password, opRefPrefix) {
		return "", errors.New("--api-token-from-1password must be an op:// secret reference")
	}
	opToken, err := readOp(ctx, tokenFrom1Password)
	if err != nil {
		return "", fmt.Errorf("failed to fetch API token from 1Password: %w", err)
	}
	return opToken, nil
}

func readOp(ctx context.Context, ref string) (string, error) {
	// -n avoids a trailing newline.
	return run(ctx, "op", "read", "-n", ref)
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", fmt.Errorf("%s failed: %s", name, exitErr.ProcessState)
		}
		return "", fmt.Errorf("failed to run %s: %w", name, err)
	}
	secret := strings.TrimSpace(string(out))
	if secret == "" {
		return "", fmt.Errorf("%s returned an empty secret", name)
	}
	return secret, nil
}
