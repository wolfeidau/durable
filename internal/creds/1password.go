// Package creds resolves the API token from command line flags.
package creds

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const opRefPrefix = "op://"

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
	opToken, err := fetchTokenFrom1Password(ctx, tokenFrom1Password)
	if err != nil {
		return "", fmt.Errorf("failed to fetch API token from 1Password: %w", err)
	}
	return opToken, nil
}

func fetchTokenFrom1Password(ctx context.Context, ref string) (string, error) {
	// -n avoids a trailing newline.
	out, err := exec.CommandContext(ctx, "op", "read", "-n", ref).Output()
	if err != nil {
		return "", expandExecErr(err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", errors.New("1Password returned an empty secret")
	}
	return token, nil
}

func expandExecErr(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("op failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
	}
	return err
}
