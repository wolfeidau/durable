package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Image is the tag BuildImage produces.
const Image = "durable-worker:spike"

// WorkerBinary is the worker's path inside the image.
const WorkerBinary = "/usr/local/bin/durable-worker"

// SandboxRoot is the image's working directory and the worker's root.
const SandboxRoot = "/sandbox"

// run executes a CLI and returns its combined output. Output goes to a file because the
// background port forward is a child that inherits and holds open any pipe.
func run(ctx context.Context, env []string, name string, args ...string) (string, error) {
	f, err := os.CreateTemp("", "openshell-*.log")
	if err != nil {
		return "", fmt.Errorf("failed to create output file: %w", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // the launcher shells out to fixed CLIs by design
	cmd.Stdout, cmd.Stderr = f, f
	if env != nil {
		cmd.Env = env
	}
	runErr := cmd.Run()
	out, readErr := os.ReadFile(f.Name())
	if readErr != nil {
		return "", fmt.Errorf("failed to read output: %w", readErr)
	}
	if runErr != nil {
		return string(out), fmt.Errorf("%s %s failed: %w: %s", name, args[0], runErr, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// BuildImage builds the worker image from the repository root.
func BuildImage(ctx context.Context) error {
	_, err := run(ctx, nil, "docker", "build", "-t", Image, "-f", "spike/openshell/Dockerfile", ".")
	return err
}

// Create starts a sandbox running the worker with a forward to port. The token is set in the
// CLI's environment only. extra is appended to the worker's arguments.
func Create(ctx context.Context, name string, port int, token string, extra ...string) error {
	addr := "127.0.0.1:" + strconv.Itoa(port)
	args := []string{
		"sandbox", "create", "--from", Image, "--name", name, "--forward", addr,
		"--detach", "--env-from", "DURABLE_WORKER_TOKEN", "--no-auto-providers",
		"--", WorkerBinary, "--addr", addr, "--root", SandboxRoot,
	}
	args = append(args, extra...)
	env := append(os.Environ(), "DURABLE_WORKER_TOKEN="+token)
	_, err := run(ctx, env, "openshell", args...)
	return err
}

// Upload copies a local path into the sandbox; dest "" means the sandbox working directory.
func Upload(ctx context.Context, name, local, dest string, noGitIgnore bool) error {
	args := []string{"sandbox", "upload"}
	if noGitIgnore {
		args = append(args, "--no-git-ignore")
	}
	args = append(args, name, local)
	if dest != "" {
		args = append(args, dest)
	}
	_, err := run(ctx, nil, "openshell", args...)
	return err
}

// ForwardStart starts a background forward from 127.0.0.1:port to the sandbox.
func ForwardStart(ctx context.Context, name string, port int) error {
	_, err := run(ctx, nil, "openshell", "forward", "start", "-d", "127.0.0.1:"+strconv.Itoa(port), name)
	return err
}

// ForwardStop stops the forward for port.
func ForwardStop(ctx context.Context, name string, port int) error {
	_, err := run(ctx, nil, "openshell", "forward", "stop", strconv.Itoa(port), name)
	return err
}

// Delete removes the sandbox and its forward.
func Delete(ctx context.Context, name string) error {
	_, err := run(ctx, nil, "openshell", "sandbox", "delete", name)
	return err
}

// Stop stops the sandbox, keeping its workspace.
func Stop(ctx context.Context, name string) error {
	_, err := run(ctx, nil, "openshell", "sandbox", "stop", name)
	return err
}

// Start starts a stopped sandbox.
func Start(ctx context.Context, name string) error {
	_, err := run(ctx, nil, "openshell", "sandbox", "start", name)
	return err
}

// Exec runs a command in the sandbox and returns its output.
func Exec(ctx context.Context, name string, command ...string) (string, error) {
	return run(ctx, nil, "openshell", append([]string{"sandbox", "exec", name, "--"}, command...)...)
}

// List returns the raw output of `openshell sandbox list` and `openshell forward list`.
func List(ctx context.Context) (sandboxes, forwards string, err error) {
	if sandboxes, err = run(ctx, nil, "openshell", "sandbox", "list"); err != nil {
		return "", "", err
	}
	if forwards, err = run(ctx, nil, "openshell", "forward", "list"); err != nil {
		return "", "", err
	}
	return sandboxes, forwards, nil
}

// WaitReady polls Info until the worker answers or timeout passes.
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, last = c.Info(rctx)
		cancel()
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return errors.Join(errors.New("worker not ready before deadline"), last)
}
