package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/worker"
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

// binPath is the real binary, built once because linking it per test dominated the suite's run time.
var binPath string

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "durable-worker-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	binPath = filepath.Join(dir, "durable-worker")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to build binary: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv(worker.TokenEnv, "")
}

func startLauncher(t *testing.T) (*sandbox.Worker, string) {
	t.Helper()
	isolate(t)
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	w, err := sandbox.LocalLauncher{Binary: binPath, Root: root, StateDir: t.TempDir()}.Start(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	return w, root
}

func toolsByName(t *testing.T, w *sandbox.Worker) map[string]durable.Tool {
	t.Helper()
	all, err := w.Client().Tools(t.Context())
	require.NoError(t, err)
	byName := map[string]durable.Tool{}
	for _, tl := range all {
		byName[tl.Spec().Name] = tl
	}
	return byName
}

func TestRefusesWithoutToken(t *testing.T) {
	isolate(t)
	var out bytes.Buffer
	cmd := exec.CommandContext(t.Context(), binPath, "--root", t.TempDir(), "--state-dir", t.TempDir())
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.NotZero(t, exitErr.ExitCode())
	require.Contains(t, out.String(), worker.TokenEnv)
}

func TestHandshakeAndGracefulShutdown(t *testing.T) {
	isolate(t)
	cmd := exec.CommandContext(t.Context(), binPath, "--root", t.TempDir(), "--state-dir", t.TempDir())
	cmd.Env = append(os.Environ(), worker.TokenEnv+"=test-token")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	br := bufio.NewReader(stdout)
	line, err := br.ReadString('\n')
	require.NoError(t, err)
	require.Regexp(t, `^durable-worker listening 127\.0\.0\.1:\d+\n$`, line)

	start := time.Now()
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	rest, err := io.ReadAll(br)
	require.NoError(t, err)
	require.Empty(t, rest, "the handshake must be the only stdout line")
	require.NoError(t, cmd.Wait())
	require.Less(t, time.Since(start), 5*time.Second)
}

func TestToolsEndToEnd(t *testing.T) {
	w, root := startLauncher(t)
	byName := toolsByName(t, w)
	ctx := t.Context()
	run := func(name, id, input string) string {
		t.Helper()
		out, err := byName[name].Execute(ctx, durable.ToolCall{ID: id, Name: name, Input: []byte(input)})
		require.NoError(t, err)
		return out
	}

	require.NoError(t, os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello world\n"), 0o600))

	require.Contains(t, run("list_files", "c1", `{"path":null}`), "hello.txt")
	require.Contains(t, run("read_file", "c2", `{"path":"hello.txt","offset":null,"limit":null}`), "hello world")
	require.Contains(t, run("grep", "c3", `{"pattern":"wor.d","path":null,"glob":null}`), "hello.txt:1:")

	run("edit_file", "c4", `{"path":"hello.txt","old_string":"world","new_string":"worker"}`)
	require.Contains(t, run("read_file", "c5", `{"path":"hello.txt","offset":null,"limit":null}`), "hello worker")

	require.Contains(t, run("bash", "c6", `{"command":"pwd && echo ok","timeout_seconds":null}`), "ok")
}

func TestStreamedBashDeliversDeltas(t *testing.T) {
	w, _ := startLauncher(t)
	bash := toolsByName(t, w)["bash"]

	var got []string
	ctx := durable.WithToolOutputSink(t.Context(), func(s string) { got = append(got, s) })
	out, err := bash.Execute(ctx, durable.ToolCall{
		ID: "s1", Name: "bash",
		Input: []byte(`{"command":"for i in 1 2 3; do echo $i; sleep 0.2; done","timeout_seconds":null}`),
	})
	require.NoError(t, err)
	require.Equal(t, "1\n2\n3\n", out)
	require.Equal(t, "1\n2\n3\n", strings.Join(got, ""))
	require.Greater(t, len(got), 1, "output should arrive in several pieces")
}

func TestCancelledBashReturnsPromptly(t *testing.T) {
	w, _ := startLauncher(t)
	bash := toolsByName(t, w)["bash"]

	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	_, err := bash.Execute(ctx, durable.ToolCall{ID: "k1", Name: "bash", Input: []byte(`{"command":"sleep 30","timeout_seconds":null}`)})
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(start), 10*time.Second)
}

func TestBashDoesNotInheritToken(t *testing.T) {
	isolate(t)
	const token = "tok-7f3a9c1e5b2d4086"
	cmd := exec.CommandContext(t.Context(), binPath, "--root", t.TempDir(), "--state-dir", t.TempDir())
	cmd.Env = append(os.Environ(), worker.TokenEnv+"="+token)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	addr := strings.TrimSpace(strings.TrimPrefix(line, "durable-worker listening "))

	c := sandbox.NewClient("http://"+addr, token, nil)
	t.Cleanup(c.CloseIdle)
	all, err := c.Tools(t.Context())
	require.NoError(t, err)
	var bash durable.Tool
	for _, tl := range all {
		if tl.Spec().Name == "bash" {
			bash = tl
		}
	}
	require.NotNil(t, bash)

	out, err := bash.Execute(t.Context(), durable.ToolCall{ID: "e1", Name: "bash", Input: []byte(`{"command":"env","timeout_seconds":null}`)})
	require.NoError(t, err)
	require.Contains(t, out, "HOME=", "env should have run")
	require.NotContains(t, out, worker.TokenEnv)
	require.NotContains(t, out, token)
}
