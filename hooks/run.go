package hooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wolfeidau/durable/tools"
)

const (
	maxStdout = 1 << 20
	maxStderr = 64 << 10
	waitDelay = 2 * time.Second
)

type output struct {
	exit   int
	stdout []byte
	stderr string
}

// capture keeps up to max bytes. With onOver set, exceeding max discards the rest and calls it.
type capture struct {
	max    int
	buf    bytes.Buffer
	over   bool
	onOver func()
}

func (c *capture) Write(p []byte) (int, error) {
	n := len(p)
	if c.over {
		return n, nil
	}
	room := c.max - c.buf.Len()
	if len(p) > room {
		if c.onOver != nil {
			c.over = true
			c.onOver()
			return n, nil
		}
		p = p[:room]
	}
	c.buf.Write(p)
	return n, nil
}

// invoke runs one hook and logs a failure. Error text is for the log, not the model.
// An ended parent ctx returns ctx.Err(), unlogged.
func (r *Runner) invoke(ctx context.Context, event, tool string, h Handler, payload []byte) (output, error) {
	out, err := r.start(ctx, h, payload)
	if err != nil && ctx.Err() == nil {
		r.logFailure(event, tool, h, err, out.stderr)
	}
	return out, err
}

func (r *Runner) start(ctx context.Context, h Handler, payload []byte) (output, error) {
	dir := r.opts.Workspace
	if !h.UseWorkspaceCwd {
		tmp, err := os.MkdirTemp("", "durable-hook-*")
		if err != nil {
			return output{}, fmt.Errorf("failed to create hook directory: %w", err)
		}
		defer os.RemoveAll(tmp)
		dir = tmp
	}

	timeout := time.Duration(h.Timeout) * time.Second
	if timeout == 0 {
		timeout = defaultTimeoutSeconds * time.Second
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout := &capture{max: maxStdout, onOver: cancel}
	stderr := &capture{max: maxStderr}

	var cmd *exec.Cmd
	if h.Args != nil {
		cmd = exec.CommandContext(tctx, h.Command, h.Args...) //nolint:gosec // the command is the user's own config
	} else {
		cmd = exec.CommandContext(tctx, "bash", "-c", h.Command) //nolint:gosec // the command is the user's own config
	}
	cmd.Dir = dir
	// With an explicit Env, os/exec does not update PWD.
	cmd.Env = append(tools.ScrubEnv(os.Environ()), "PWD="+dir)
	// os/exec ignores EPIPE here, so a hook that never reads stdin can't block us.
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Bounds Wait when a background child keeps the output pipes open.
	cmd.WaitDelay = waitDelay

	if err := cmd.Start(); err != nil {
		return output{}, fmt.Errorf("hook %q failed to start: %w", h.Command, err)
	}
	werr := cmd.Wait()
	res := output{stdout: stdout.buf.Bytes(), stderr: stderr.buf.String()}

	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if stdout.over {
		return res, fmt.Errorf("hook %q wrote more than %d bytes to stdout", h.Command, maxStdout)
	}
	if errors.Is(tctx.Err(), context.DeadlineExceeded) {
		return res, fmt.Errorf("hook %q timed out after %s", h.Command, timeout)
	}

	var exitErr *exec.ExitError
	switch {
	case werr == nil:
		return res, nil
	case errors.Is(werr, exec.ErrWaitDelay):
		// The hook exited but a child still holds the pipes.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return res, nil
	case errors.As(werr, &exitErr) && exitErr.ExitCode() == 2:
		res.exit = 2
		return res, nil
	case errors.As(werr, &exitErr):
		return res, fmt.Errorf("hook %q failed with exit status %d", h.Command, exitErr.ExitCode())
	}
	return res, fmt.Errorf("hook %q failed: %w", h.Command, werr)
}

// logFailure is best effort: a log write failure must not fail the hook.
func (r *Runner) logFailure(event, tool string, h Handler, cause error, stderr string) {
	if r.opts.LogDir == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(r.opts.LogDir, "hooks.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	entry := fmt.Sprintf("%s event=%s tool=%s command=%q error=%q\n", time.Now().UTC().Format(time.RFC3339), event, tool, h.Command, cause.Error())
	if s := strings.TrimSpace(stderr); s != "" {
		entry += "stderr:\n" + s + "\n"
	}
	_, _ = f.WriteString(entry)
}
