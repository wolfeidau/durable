// Command durable-worker runs the coding tools inside a sandbox and serves them over Connect.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/alecthomas/kong"

	"github.com/wolfeidau/durable/internal/worker"
	"github.com/wolfeidau/durable/tools"
)

type cli struct {
	Addr     string `help:"Address to listen on." default:"127.0.0.1:0"`
	Root     string `help:"Workspace root the tools are confined to. Defaults to the current directory." type:"path"`
	StateDir string `name:"state-dir" help:"Per-call state that must survive a crash, kept outside the root. Defaults to durable-worker under $XDG_STATE_HOME, or under the temp directory." type:"path"`
}

func (c *cli) Run(ctx context.Context, logger *slog.Logger) error {
	token := os.Getenv(worker.TokenEnv)
	if token == "" {
		return fmt.Errorf("%s must be set to a non-empty token", worker.TokenEnv)
	}
	// Tool subprocesses inherit the environment, so `env` would otherwise print the token into tool output.
	if err := os.Unsetenv(worker.TokenEnv); err != nil {
		return fmt.Errorf("failed to unset %s: %w", worker.TokenEnv, err)
	}
	root := c.Root
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get working directory: %w", err)
		}
		root = cwd
	}
	stateDir := c.StateDir
	if stateDir == "" {
		stateDir = defaultStateDir()
	}

	all, err := tools.All(root, stateDir)
	if err != nil {
		return fmt.Errorf("failed to build tools: %w", err)
	}
	h, err := worker.Handler(worker.Config{Tools: all, Root: root, Token: token})
	if err != nil {
		return fmt.Errorf("failed to build handler: %w", err)
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", c.Addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", c.Addr, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	srv.Protocols = new(http.Protocols)
	srv.Protocols.SetHTTP1(true)
	srv.Protocols.SetUnencryptedHTTP2(true)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	logger.Info("serving", slog.String("addr", ln.Addr().String()), slog.String("root", root))
	// The launcher reads this one line to learn the port, so nothing else goes to stdout.
	fmt.Fprintf(os.Stdout, "durable-worker listening %s\n", ln.Addr())

	select {
	case err := <-errc:
		return fmt.Errorf("failed to serve: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		// Open streams may outlast the grace period.
		_ = srv.Close()
		return fmt.Errorf("failed to shut down: %w", err)
	}
	return nil
}

func defaultStateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "durable-worker")
	}
	return filepath.Join(os.TempDir(), "durable-worker-"+strconv.Itoa(os.Getuid()))
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	var c cli
	k := kong.Parse(&c,
		kong.Name("durable-worker"),
		kong.Description("Run the coding tools for a durable server inside a sandbox."),
		kong.BindTo(ctx, (*context.Context)(nil)),
		kong.Bind(logger),
	)
	err := k.Run()
	stop()
	if err != nil {
		logger.Error("durable-worker failed", slog.Any("error", err))
		os.Exit(1)
	}
}
