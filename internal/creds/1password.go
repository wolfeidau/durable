// Package creds resolves secrets from command line flags and the environment.
package creds

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	ghSource   = "gh"
	envPrefix  = "env:"
	filePrefix = "file:"
)

// EnvRefPattern is env:NAME, unanchored so callers can embed it.
const EnvRefPattern = `env:[A-Za-z_][A-Za-z0-9_]*`

// FileRefPattern is file:PATH where PATH is absolute or starts with ~/, unanchored so callers can embed it.
const FileRefPattern = `file:(/|~/).+`

// OpRefPattern is op://vault/item/[section/]field with an optional ?attribute=..., unanchored so callers can embed it.
const OpRefPattern = `op://[^/?]+/[^/?]+/[^/?]+(/[^/?]+)?(\?.+)?`

// Anchored secret reference patterns; TokenRefPattern also accepts "gh".
const (
	RefPattern      = `^(` + OpRefPattern + `|` + EnvRefPattern + `|` + FileRefPattern + `)$`
	TokenRefPattern = `^(` + OpRefPattern + `|gh|` + EnvRefPattern + `|` + FileRefPattern + `)$`

	RefHint      = "an op:// secret reference, env:NAME or file:PATH"
	TokenRefHint = "an op:// secret reference, gh, env:NAME or file:PATH" //nolint:gosec // hint text, not a credential
)

var (
	envRefRE   = regexp.MustCompile(`^` + EnvRefPattern + `$`)
	fileRefRE  = regexp.MustCompile(`^` + FileRefPattern + `$`)
	opRefRE    = regexp.MustCompile(`^` + OpRefPattern + `$`)
	refRE      = regexp.MustCompile(RefPattern)
	tokenRefRE = regexp.MustCompile(TokenRefPattern)
)

// IsOpRef reports whether s is a well-formed 1Password secret reference.
func IsOpRef(s string) bool {
	return opRefRE.MatchString(s)
}

// IsRef reports whether s is a valid secret reference (op://, env:NAME or file:PATH).
func IsRef(s string) bool {
	return refRE.MatchString(s)
}

// IsTokenRef reports whether s is a valid token secret reference (op://, gh, env:NAME or file:PATH).
func IsTokenRef(s string) bool {
	return tokenRefRE.MatchString(s)
}

// Source says where a secret comes from.
type Source struct {
	// Flag names the option that sets Value, for errors.
	Flag string
	// Env is read when Value is empty.
	Env string
	// Value is an op:// reference, the literal "gh" for the GitHub CLI's token, env:NAME, file:PATH (absolute or ~/), or empty.
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
	case IsOpRef(s.Value):
		secret, err := readOp(ctx, s.Value)
		if err != nil {
			return "", fmt.Errorf("failed to read %s from 1Password: %w", s.Flag, err)
		}
		return secret, nil
	case envRefRE.MatchString(s.Value):
		name := strings.TrimPrefix(s.Value, envPrefix)
		secret := strings.TrimSpace(os.Getenv(name))
		if secret == "" {
			return "", fmt.Errorf("no secret for %s: set %s", s.Flag, name)
		}
		return secret, nil
	case fileRefRE.MatchString(s.Value):
		return readFile(s)
	case s.Value != "":
		return "", fmt.Errorf("%s must be %s", s.Flag, TokenRefHint)
	}
	secret := strings.TrimSpace(os.Getenv(s.Env))
	if secret == "" {
		return "", fmt.Errorf("no token: set %s or %s", s.Flag, s.Env)
	}
	return secret, nil
}

func readFile(s Source) (string, error) {
	path := strings.TrimPrefix(s.Value, filePrefix)
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to read %s from file %s: %w", s.Flag, path, err)
		}
		path = filepath.Join(home, rest)
	}
	b, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own config
	if err != nil {
		return "", fmt.Errorf("failed to read %s from file %s: %w", s.Flag, path, err)
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return "", fmt.Errorf("no secret for %s: %s is empty", s.Flag, path)
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
	if !IsOpRef(tokenFrom1Password) {
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
