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
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"

	"github.com/wolfeidau/durable/internal/creds"
	"github.com/wolfeidau/durable/internal/server"
)

type serveCmd struct {
	Addr string `help:"Address to listen on." default:"127.0.0.1:7878"`

	IdleTimeout           time.Duration `name:"idle-timeout" help:"Close a session with no clients and no work after this long." default:"5m"`
	OpenTimeout           time.Duration `name:"open-timeout" help:"Give up opening a session after this long, for example while durable chat holds it." default:"10s"`
	Heartbeat             time.Duration `name:"heartbeat" help:"Send a heartbeat on an idle event stream this often." default:"20s"`
	APIToken              string        `name:"api-token" help:"API key for the provider. Visible in the process list and shell history; prefer --api-token-from-1password."`
	APITokenFrom1Password string        `name:"api-token-from-1password" placeholder:"op://..." help:"1Password secret reference for the provider API key. Without a key flag, openrouter reads OPENROUTER_API_KEY."`
}

type cli struct {
	Serve       serveCmd       `cmd:"" default:"withargs" help:"Run the server."`
	URL         urlCmd         `cmd:"" name:"url" help:"Print the browser link for the running server."`
	RotateToken rotateTokenCmd `cmd:"" name:"rotate-token" help:"Replace the stored token. It takes effect when the server next starts."`
}

type urlCmd struct{}

type rotateTokenCmd struct{}

const tokenFile = "server-token"

type serverInfo struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
}

func (c *serveCmd) Run(ctx context.Context, logger *slog.Logger) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to find home directory: %w", err)
	}

	// Resolved once so a 1Password reference isn't read on every session open.
	apiToken, err := creds.ResolveAPIToken(ctx, c.APIToken, c.APITokenFrom1Password)
	if err != nil {
		return fmt.Errorf("failed to resolve API token: %w", err)
	}

	token, err := loadOrCreateToken(filepath.Join(home, ".durable", tokenFile))
	if err != nil {
		return err
	}

	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", c.Addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", c.Addr, err)
	}
	addr := ln.Addr().String()
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
		Home:        home,
		IdleTimeout: c.IdleTimeout,
		OpenTimeout: c.OpenTimeout,
		APIToken:    apiToken,
		Logger:      logger,
	})
	defer registry.Close()
	draining := make(chan struct{})
	srv := &http.Server{
		Handler:           server.New(server.Config{Home: home, Token: token, Addr: addr, Logger: logger, Registry: registry, Heartbeat: c.Heartbeat, Draining: draining}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}
	// A stream never goes idle, so Shutdown would wait out the grace period for it.
	srv.RegisterOnShutdown(func() { close(draining) })
	srv.Protocols = new(http.Protocols)
	srv.Protocols.SetHTTP1(true)
	srv.Protocols.SetUnencryptedHTTP2(true)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	logger.Info("serving", slog.String("url", "http://"+addr), slog.String("hint", "run durable-server url for the browser link"))

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
	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("failed to encode server info: %w", err)
	}
	return writeFileAtomic(path, data)
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to chmod %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("failed to replace %s: %w", path, err)
	}
	return nil
}

// loadOrCreateToken keeps the token across restarts so a restart doesn't sign the browser out.
func loadOrCreateToken(path string) (string, error) {
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		token := rand.Text()
		if err := writeFileAtomic(path, []byte(token+"\n")); err != nil {
			return "", err
		}
		return token, nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to stat %s: %w", path, err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is accessible by other users (mode %04o); run chmod 600 %s", path, st.Mode().Perm(), path)
	}
	data, err := os.ReadFile(path) //nolint:gosec // fixed name inside the durable directory
	if err != nil {
		return "", fmt.Errorf("failed to read %s: %w", path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("%s is empty; delete it or run durable-server rotate-token", path)
	}
	return token, nil
}

func (*urlCmd) Run(ctx context.Context) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to find home directory: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".durable", "server.json")) //nolint:gosec // fixed name inside the durable directory
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("no server running")
	}
	if err != nil {
		return fmt.Errorf("failed to read server info: %w", err)
	}
	var info serverInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return fmt.Errorf("failed to parse server info: %w", err)
	}
	dialer := net.Dialer{Timeout: time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", info.Addr)
	if err != nil {
		return fmt.Errorf("no server running at %s (server.json is left over): %w", info.Addr, err)
	}
	_ = conn.Close()
	fmt.Printf("http://%s/#token=%s\n", info.Addr, info.Token)
	return nil
}

func (*rotateTokenCmd) Run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to find home directory: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(home, ".durable", tokenFile), []byte(rand.Text()+"\n")); err != nil {
		return err
	}
	fmt.Println("Token rotated. It takes effect when the server next starts; browsers need the new link from durable-server url.")
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
