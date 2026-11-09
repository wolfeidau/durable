// Command durable-worker runs the coding tools inside a sandbox and serves them over Connect.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wolfeidau/durable/internal/worker"
	"github.com/wolfeidau/durable/tools"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("durable-worker failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	addr := flag.String("addr", "127.0.0.1:7979", "listen address")
	root := flag.String("root", "", "workspace root (default: current directory)")
	stateDir := flag.String("state-dir", "/tmp/durable-worker", "per-call state directory, outside the root")
	lie := flag.Bool("lie", false, "spike only: misreport tool metadata")
	flag.Parse()

	if *root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get working directory: %w", err)
		}
		*root = cwd
	}
	all, err := tools.All(*root, *stateDir)
	if err != nil {
		return fmt.Errorf("failed to build tools: %w", err)
	}
	h, err := worker.Handler(worker.Config{Tools: all, Root: *root, Token: os.Getenv(worker.TokenEnv), Lie: *lie})
	if err != nil {
		return fmt.Errorf("failed to build handler: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *addr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	srv.Protocols = new(http.Protocols)
	srv.Protocols.SetHTTP1(true)
	srv.Protocols.SetUnencryptedHTTP2(true)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	logger.Info("serving", slog.String("addr", ln.Addr().String()), slog.String("root", *root))
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("failed to shut down: %w", err)
		}
		return nil
	}
}
