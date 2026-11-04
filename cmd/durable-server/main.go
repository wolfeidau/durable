// Command durable-server serves the durable Connect API and web UI.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/alecthomas/kong"

	"github.com/wolfeidau/durable/internal/server"
)

type cli struct {
	Addr string `help:"Address to listen on." default:"127.0.0.1:7878"`

	IdleTimeout           time.Duration `name:"idle-timeout" help:"Close a session with no clients and no work after this long." default:"5m"`
	APIToken              string        `name:"api-token" help:"API key for the provider. Visible in the process list and shell history; prefer --api-token-from-1password."`
	APITokenFrom1Password string        `name:"api-token-from-1password" placeholder:"op://..." help:"1Password secret reference for the provider API key. Without a key flag, openrouter reads OPENROUTER_API_KEY."`
}

type serverInfo struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
}

func (c *cli) Run(ctx context.Context, logger *slog.Logger) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to find home directory: %w", err)
	}

	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", c.Addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", c.Addr, err)
	}
	addr := ln.Addr().String()
	token := rand.Text()

	infoPath := filepath.Join(home, ".durable", "server.json")
	if err := writeServerInfo(infoPath, serverInfo{Addr: addr, Token: token}); err != nil {
		_ = ln.Close()
		return err
	}
	defer os.Remove(infoPath)

	// Requests drain within the grace period, and streams end when the base context is cancelled.
	baseCtx, cancelBase := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelBase()
	// Runs outlive requests, so the registry's context is cancelled only after the server has drained.
	registry := server.NewRegistry(context.WithoutCancel(ctx), server.RegistryConfig{
		Home:                  home,
		IdleTimeout:           c.IdleTimeout,
		APIToken:              c.APIToken,
		APITokenFrom1Password: c.APITokenFrom1Password,
		Logger:                logger,
	})
	defer registry.Close()
	srv := &http.Server{
		Handler:           server.New(server.Config{Home: home, Token: token, Addr: addr, Logger: logger, Registry: registry}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}
	srv.Protocols = new(http.Protocols)
	srv.Protocols.SetHTTP1(true)
	srv.Protocols.SetUnencryptedHTTP2(true)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	logger.Info("serving", slog.String("url", "http://"+addr))

	select {
	case err := <-errc:
		return fmt.Errorf("failed to serve: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	cancelBase()
	// Interrupted runs resume on the next open.
	registry.Close()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		// Open streams may outlast the grace period.
		_ = srv.Close()
		return fmt.Errorf("failed to shut down: %w", err)
	}
	return nil
}

// writeServerInfo replaces path atomically, so a reader never sees a partial token.
func writeServerInfo(path string, info serverInfo) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("failed to create %s: %w", filepath.Dir(path), err)
	}
	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("failed to encode server info: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "server.json.*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to chmod server info: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write server info: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close server info: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("failed to replace %s: %w", path, err)
	}
	return nil
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	var c cli
	k := kong.Parse(&c,
		kong.Name("durable-server"),
		kong.Description("Serve the durable API and web UI."),
		kong.BindTo(ctx, (*context.Context)(nil)),
		kong.Bind(logger),
	)
	err := k.Run()
	stop()
	if err != nil {
		logger.Error("server failed", slog.Any("error", err))
		os.Exit(1)
	}
}
