package commands

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"

	"github.com/wolfeidau/durable/internal/githost"
	"github.com/wolfeidau/durable/internal/sandbox"
)

// maxCloneMsg caps the git output a client sees.
const maxCloneMsg = 200

// CloneError is a failed clone of a repo session's branch. Msg is git's last output line, already sanitised.
type CloneError struct{ Msg string }

func (e *CloneError) Error() string { return "failed to clone the repository: " + e.Msg }

// cloneScript clones into the workspace unless it already has files. It clones into a staging directory and then moves
// the files into place, so a clone killed partway leaves only that directory, which the next open removes and retries.
// Arguments are positional so none is interpolated.
const cloneScript = `rm -rf .durable-clone
if [ -n "$(ls -A)" ]; then exit 0; fi
git clone --quiet --branch "$1" -- "$2" .durable-clone || exit
for f in .durable-clone/* .durable-clone/.[!.]* .durable-clone/..?*; do
  if [ -e "$f" ] || [ -L "$f" ]; then mv "$f" . || exit; fi
done
rmdir .durable-clone`

// ensureClone clones url's branch into the sandbox workspace when the workspace is empty, so a deleted sandbox gets its repository back.
func ensureClone(ctx context.Context, client *sandbox.Client, url, branch string) error {
	code, stderr, err := client.Run(ctx, sandbox.StdioSpec{
		Command:      "sh",
		Args:         []string{"-c", cloneScript, "sh", branch, url},
		Env:          map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		WorkspaceCwd: true,
	})
	if err != nil {
		return fmt.Errorf("failed to run git clone: %w", err)
	}
	if code != 0 {
		msg := lastLine(stderr)
		if msg == "" {
			msg = fmt.Sprintf("git exited with status %d", code)
		}
		return &CloneError{Msg: msg}
	}
	return nil
}

// lastLine returns the last non-empty line of git's output without control characters, because that output is untrusted.
func lastLine(b []byte) string {
	lines := strings.FieldsFunc(string(b), func(r rune) bool { return r == '\n' || r == '\r' })
	for _, l := range slices.Backward(lines) {
		l = strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, l))
		if l == "" {
			continue
		}
		if len(l) > maxCloneMsg {
			l = strings.ToValidUTF8(l[:maxCloneMsg], "")
		}
		return l
	}
	return ""
}

// repoAccess returns the sandbox's access to the session's repository: with the host's token when the server has one, else anonymous.
func (c AssembleConfig) repoAccess() (sandbox.GitAccess, error) {
	for _, kind := range slices.Sorted(maps.Keys(c.Sandbox.GitHosts)) {
		h := c.Sandbox.GitHosts[kind]
		if _, err := h.ParseRepo(c.Repo.String()); err != nil {
			continue
		}
		access := h.Access(c.Repo)
		if !c.Sandbox.GitTokens[kind] {
			access = githost.Anonymous(access)
		}
		return access, nil
	}
	return sandbox.GitAccess{}, errors.New("no git host is configured for the repository")
}
