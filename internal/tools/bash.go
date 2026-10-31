package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/wolfeidau/durable"
)

const (
	defaultTimeoutSeconds = 120
	maxTimeoutSeconds     = 600
	maxOutputBytes        = 30 * 1024
	waitDelay             = 2 * time.Second
)

func newBash(root string) durable.Tool {
	return tool{
		spec: durable.ToolSpec{
			Name: "bash",
			Description: "Run a command with `bash -c` in the workspace root. stdout and stderr are combined; there is no stdin, so interactive commands fail. " +
				"A non-zero exit is reported as an error including the exit code and output. Output over 30KB keeps the first and last 15KB with a marker for the omitted middle. " +
				"Default timeout is 120 seconds, maximum 600; on timeout the whole process group is killed.",
			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"command":{"type":"string","description":"Shell command to run."},` +
				`"timeout_seconds":{"type":["integer","null"],"description":"Timeout in seconds (1-600), or null for 120."}},` +
				`"required":["command","timeout_seconds"],"additionalProperties":false}`),
		},
		replay: durable.ReplayNever,
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			var in struct {
				Command        string `json:"command"`
				TimeoutSeconds *int   `json:"timeout_seconds"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			if in.Command == "" {
				return "", errors.New("command must not be empty")
			}
			secs := defaultTimeoutSeconds
			if in.TimeoutSeconds != nil {
				secs = *in.TimeoutSeconds
			}
			if secs < 1 || secs > maxTimeoutSeconds {
				return "", fmt.Errorf("timeout_seconds must be between 1 and %d, got %d", maxTimeoutSeconds, secs)
			}
			return runBash(ctx, root, in.Command, time.Duration(secs)*time.Second)
		},
	}
}

func runBash(ctx context.Context, root, command string, timeout time.Duration) (string, error) {
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out := newBoundedWriter(maxOutputBytes)
	cmd := exec.CommandContext(tctx, "bash", "-c", command)
	cmd.Dir = root
	cmd.Env = scrubEnv(os.Environ())
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Bounds Wait when a surviving child keeps the output pipes open.
	cmd.WaitDelay = waitDelay

	err := cmd.Run()

	// Parent cancellation means the process is going away; it is not a tool result.
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if errors.Is(tctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("command timed out after %s; output so far:\n%s", timeout, out.String())
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil, errors.Is(err, exec.ErrWaitDelay):
		return out.String(), nil
	case errors.As(err, &exitErr):
		return "", fmt.Errorf("command exited with code %d; output:\n%s", exitErr.ExitCode(), out.String())
	default:
		return "", fmt.Errorf("failed to run command: %w", err)
	}
}

// scrubEnv drops credentials, because command output reaches the model and the session database.
func scrubEnv(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "OP_")
	})
}

// boundedWriter keeps the first and last max/2 bytes written and counts the rest.
type boundedWriter struct {
	half    int
	head    []byte
	tail    []byte
	omitted int
}

func newBoundedWriter(max int) *boundedWriter {
	return &boundedWriter{half: max / 2}
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if room := w.half - len(w.head); room > 0 {
		take := min(room, len(p))
		w.head = append(w.head, p[:take]...)
		p = p[take:]
	}
	if len(p) == 0 {
		return n, nil
	}
	w.tail = append(w.tail, p...)
	// Trim lazily so appends stay amortised.
	if len(w.tail) > 2*w.half {
		drop := len(w.tail) - w.half
		w.omitted += drop
		w.tail = append(w.tail[:0], w.tail[drop:]...)
	}
	return n, nil
}

func (w *boundedWriter) String() string {
	tail := w.tail
	omitted := w.omitted
	if len(tail) > w.half {
		omitted += len(tail) - w.half
		tail = tail[len(tail)-w.half:]
	}
	if omitted == 0 {
		return string(w.head) + string(tail)
	}
	return fmt.Sprintf("%s\n... [%d bytes omitted] ...\n%s", w.head, omitted, tail)
}
