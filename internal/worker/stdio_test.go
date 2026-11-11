package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	workerv1 "github.com/wolfeidau/durable/api/durable/worker/v1"
	"github.com/wolfeidau/durable/api/durable/worker/v1/workerv1connect"
)

func startRoot(t *testing.T, root string) workerv1connect.ToolServiceClient {
	t.Helper()
	h, err := Handler(Config{Root: root, Token: testToken})
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(h)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)

	transport := &http.Transport{Protocols: new(http.Protocols)}
	transport.Protocols.SetUnencryptedHTTP2(true)
	t.Cleanup(transport.CloseIdleConnections)
	return workerv1connect.NewToolServiceClient(&http.Client{Transport: transport}, srv.URL)
}

func openStdio(ctx context.Context, c workerv1connect.ToolServiceClient, start *workerv1.StdioStart) (*connect.BidiStreamForClient[workerv1.StdioRequest, workerv1.StdioResponse], error) {
	s := c.Stdio(ctx)
	s.RequestHeader().Set("Authorization", "Bearer "+testToken)
	return s, s.Send(&workerv1.StdioRequest{Event: &workerv1.StdioRequest_Start{Start: start}})
}

type stdioOut struct {
	stdout, stderr []byte
	frames         []int // sizes of output frames
	exit           *int32
	events         int
}

// collect reads until the stream ends and checks that nothing follows the exit code.
func collect(t *testing.T, s *connect.BidiStreamForClient[workerv1.StdioRequest, workerv1.StdioResponse]) stdioOut {
	t.Helper()
	var out stdioOut
	for {
		msg, err := s.Receive()
		if err != nil {
			return out
		}
		require.Nil(t, out.exit, "frame after exit_code")
		out.events++
		switch ev := msg.Event.(type) {
		case *workerv1.StdioResponse_Stdout:
			out.stdout = append(out.stdout, ev.Stdout...)
			out.frames = append(out.frames, len(ev.Stdout))
		case *workerv1.StdioResponse_Stderr:
			out.stderr = append(out.stderr, ev.Stderr...)
			out.frames = append(out.frames, len(ev.Stderr))
		case *workerv1.StdioResponse_ExitCode:
			out.exit = &ev.ExitCode
		}
	}
}

func TestStdioRoundTrip(t *testing.T) {
	c := startRoot(t, t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	s, err := openStdio(ctx, c, &workerv1.StdioStart{Command: "cat"})
	require.NoError(t, err)
	require.NoError(t, s.Send(&workerv1.StdioRequest{Event: &workerv1.StdioRequest_Stdin{Stdin: []byte("hello\n")}}))
	require.NoError(t, s.CloseRequest())
	out := collect(t, s)
	require.Equal(t, "hello\n", string(out.stdout))
	require.NotNil(t, out.exit)
	require.Equal(t, int32(0), *out.exit)
}

func TestStdioStderrAndExitCode(t *testing.T) {
	c := startRoot(t, t.TempDir())
	s, err := openStdio(t.Context(), c, &workerv1.StdioStart{Command: "sh", Args: []string{"-c", "echo out; echo err >&2; exit 3"}})
	require.NoError(t, err)
	require.NoError(t, s.CloseRequest())
	out := collect(t, s)
	require.Equal(t, "out\n", string(out.stdout))
	require.Equal(t, "err\n", string(out.stderr))
	require.NotNil(t, out.exit)
	require.Equal(t, int32(3), *out.exit)
}

func TestStdioChunksLargeOutput(t *testing.T) {
	c := startRoot(t, t.TempDir())
	s, err := openStdio(t.Context(), c, &workerv1.StdioStart{Command: "head", Args: []string{"-c", "200000", "/dev/zero"}})
	require.NoError(t, err)
	require.NoError(t, s.CloseRequest())
	out := collect(t, s)
	require.Len(t, out.stdout, 200000)
	for _, n := range out.frames {
		require.LessOrEqual(t, n, stdioChunk)
	}
}

func TestStdioEnvAndCwd(t *testing.T) {
	t.Setenv(TokenEnv, "worker-secret")
	root := t.TempDir()
	c := startRoot(t, root)
	run := func(workspace bool) []string {
		s, err := openStdio(t.Context(), c, &workerv1.StdioStart{
			Command: "sh", Args: []string{"-c", `echo "$FOO"; pwd -P; echo "token=$DURABLE_WORKER_TOKEN"`},
			Env: map[string]string{"FOO": "bar"}, WorkspaceCwd: workspace,
		})
		require.NoError(t, err)
		require.NoError(t, s.CloseRequest())
		return strings.Split(strings.TrimSpace(string(collect(t, s).stdout)), "\n")
	}

	got := run(true)
	want, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.Equal(t, []string{"bar", want, "token="}, got)

	got = run(false)
	require.Equal(t, "bar", got[0])
	require.NotEqual(t, want, got[1])
	require.Equal(t, "token=", got[2])
	require.NoDirExists(t, got[1], "the temporary directory is removed after the process")
}

func TestStdioEndOfStreamKillsProcessGroup(t *testing.T) {
	c := startRoot(t, t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	// The shell waits on a background child, so only a group kill ends both.
	s, err := openStdio(ctx, c, &workerv1.StdioStart{Command: "sh", Args: []string{"-c", "sleep 300 & echo $!; wait"}})
	require.NoError(t, err)
	var line []byte
	for !strings.Contains(string(line), "\n") {
		msg, err := s.Receive()
		require.NoError(t, err)
		line = append(line, msg.GetStdout()...)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(line)))
	require.NoError(t, err)
	require.NoError(t, syscall.Kill(pid, 0))

	require.NoError(t, s.CloseRequest())
	out := collect(t, s)
	require.NotNil(t, out.exit)
	require.Eventually(t, func() bool { return syscall.Kill(pid, 0) != nil }, 5*time.Second, 20*time.Millisecond, "child survived the stream")
}

func TestStdioDroppedClientKillsProcess(t *testing.T) {
	c := startRoot(t, t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	s, err := openStdio(ctx, c, &workerv1.StdioStart{Command: "sh", Args: []string{"-c", "echo $$; sleep 300"}})
	require.NoError(t, err)
	var line []byte
	for !strings.Contains(string(line), "\n") {
		msg, err := s.Receive()
		require.NoError(t, err)
		line = append(line, msg.GetStdout()...)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(line)))
	require.NoError(t, err)
	cancel()
	// The client only acts on the cancel inside a call, as the host's reader loop does.
	_, _ = s.Receive()
	require.Eventually(t, func() bool { return syscall.Kill(pid, 0) != nil }, 10*time.Second, 20*time.Millisecond, "process survived the dropped client")
}

func TestStdioFirstRequestMustStart(t *testing.T) {
	c := startRoot(t, t.TempDir())
	for name, first := range map[string]*workerv1.StdioRequest{
		"stdin":      {Event: &workerv1.StdioRequest_Stdin{Stdin: []byte("x")}},
		"empty":      {},
		"no command": {Event: &workerv1.StdioRequest_Start{Start: &workerv1.StdioStart{}}},
	} {
		t.Run(name, func(t *testing.T) {
			s := c.Stdio(t.Context())
			s.RequestHeader().Set("Authorization", "Bearer "+testToken)
			_ = s.Send(first)
			_, err := s.Receive()
			require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
		})
	}
}

func TestStdioUnknownCommand(t *testing.T) {
	c := startRoot(t, t.TempDir())
	s, err := openStdio(t.Context(), c, &workerv1.StdioStart{Command: "durable-no-such-command"})
	require.NoError(t, err)
	_, err = s.Receive()
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
}

func TestStdioRequiresToken(t *testing.T) {
	c := startRoot(t, t.TempDir())
	for name, header := range map[string]string{"none": "", "wrong": "Bearer nope"} {
		t.Run(name, func(t *testing.T) {
			s := c.Stdio(t.Context())
			if header != "" {
				s.RequestHeader().Set("Authorization", header)
			}
			_ = s.Send(&workerv1.StdioRequest{Event: &workerv1.StdioRequest_Start{Start: &workerv1.StdioStart{Command: "true"}}})
			_, err := s.Receive()
			require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
		})
	}
}
