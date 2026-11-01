package tools

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
)

func recoverOf(t *testing.T, e env) string {
	t.Helper()
	r, ok := e.tools["bash"].(durable.Recoverer)
	require.True(t, ok)
	return r.Recover(context.Background(), durable.ToolCall{ID: "1", Name: "bash"})
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// startGroup runs a command in its own process group, reaped in the background so a dead group is really gone.
func startGroup(t *testing.T, extra *os.File) int {
	t.Helper()
	cmd := exec.Command("bash", "-c", "exec sleep 60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if extra != nil {
		cmd.ExtraFiles = []*os.File{extra}
	}
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	return pid
}

// startOrphan simulates an agent that died: the command holds the lock and the parent's descriptor is closed.
func startOrphan(t *testing.T, path string, recordedGroup func(pid int) string) int {
	t.Helper()
	lock, err := openLock(path)
	require.NoError(t, err)
	pid := startGroup(t, lock.f)
	if recordedGroup == nil {
		require.NoError(t, lock.writeGroup(pid))
	} else {
		_, err := lock.f.WriteAt([]byte(recordedGroup(pid)), 0)
		require.NoError(t, err)
	}
	require.NoError(t, lock.f.Close())
	return pid
}

func TestRecoverStopsLiveOrphan(t *testing.T) {
	e := newEnv(t)
	pid := startOrphan(t, e.bashState(), nil)
	require.True(t, alive(pid))

	require.Equal(t, OrphanStopped, recoverOf(t, e))
	require.Eventually(t, func() bool { return !alive(pid) }, 5*time.Second, 20*time.Millisecond)
	require.NoFileExists(t, e.bashState())

	require.Empty(t, recoverOf(t, e), "recover must be idempotent")
}

func TestBashRemovesStateFile(t *testing.T) {
	cases := map[string]struct {
		command string
		timeout any
		cancel  bool
	}{
		"success": {command: "echo hi", timeout: nil},
		"error":   {command: "exit 3", timeout: nil},
		"timeout": {command: "sleep 30", timeout: 1},
		"cancel":  {command: "sleep 30", timeout: nil, cancel: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				time.AfterFunc(300*time.Millisecond, cancel)
			}
			_, _ = e.runCtx(t, ctx, "bash", map[string]any{"command": tc.command, "timeout_seconds": tc.timeout})
			require.NoFileExists(t, e.bashState())
			require.Empty(t, recoverOf(t, e))
		})
	}
}

func TestBashStateFileFailureDoesNotRunCommand(t *testing.T) {
	e := newEnv(t)
	marker := e.root + "/marker"
	require.NoError(t, os.RemoveAll(e.state+"/bash"))
	_, err := e.run(t, "bash", map[string]any{"command": "touch " + marker, "timeout_seconds": nil})
	require.ErrorContains(t, err, "state file")
	require.NoFileExists(t, marker)
}

func TestRecoverIgnoresReusedGroupID(t *testing.T) {
	e := newEnv(t)
	unrelated := startGroup(t, nil)

	// Unlocked file naming a live group: the command is dead, so the ID must not be signalled.
	require.NoError(t, os.WriteFile(e.bashState(), []byte(strconv.Itoa(unrelated)), 0o600))

	require.Empty(t, recoverOf(t, e))
	time.Sleep(100 * time.Millisecond)
	require.True(t, alive(unrelated))
	require.NoFileExists(t, e.bashState())
}

func TestRecoverHolderOutsideRecordedGroup(t *testing.T) {
	cases := map[string]func(pid int) string{
		"other group": func(int) string { return strconv.Itoa(startGroup(t, nil)) },
		"zero":        func(int) string { return "0" },
		"one":         func(int) string { return "1" },
		"negative":    func(int) string { return "-1" },
		"empty":       func(int) string { return "" },
		"garbage":     func(int) string { return "abc" },
	}
	for name, record := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			holder := startOrphan(t, e.bashState(), record)

			require.Equal(t, OrphanRunning, recoverOf(t, e))
			require.True(t, alive(holder), "holder must be left alone")
			require.FileExists(t, e.bashState())
		})
	}
}

func TestRecoverHonoursContext(t *testing.T) {
	e := newEnv(t)
	holder := startOrphan(t, e.bashState(), func(int) string { return strconv.Itoa(startGroup(t, nil)) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Empty(t, recoverBash(ctx, e.bashState()))
	require.True(t, alive(holder))
}
