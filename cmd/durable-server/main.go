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

	"github.com/wolfeidau/durable/internal/commands"
	"github.com/wolfeidau/durable/internal/config"
	"github.com/wolfeidau/durable/internal/creds"
	"github.com/wolfeidau/durable/internal/githost"
	"github.com/wolfeidau/durable/internal/githost/github"
	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/server"
)

// serveCmd's flags override ~/.durable/config.json, so none has a default here: zero means not given.
type serveCmd struct {
	Addr string `help:"Address to listen on. Default 127.0.0.1:7878."`

	IdleTimeout           time.Duration `name:"idle-timeout" help:"Close a session with no clients and no work after this long. Default 5m."`
	OpenTimeout           time.Duration `name:"open-timeout" help:"Give up opening a session after this long, for example while durable chat holds it. Default 10s."`
	Heartbeat             time.Duration `name:"heartbeat" help:"Send a heartbeat on an idle event stream this often. Default 20s."`
	Provider              string        `help:"Model provider for new sessions: anthropic or openrouter. Empty uses anthropic. Existing sessions keep their pin."`
	Protocol              string        `help:"Wire protocol for new sessions: anthropic-messages or openai-responses. Empty uses the provider's default (anthropic: anthropic-messages, openrouter: openai-responses)."`
	Model                 string        `help:"Model name for new sessions. Required for new openrouter sessions, which pin it. For anthropic, empty uses the built-in default."`
	APIToken              string        `name:"api-token" help:"API key for the provider. Visible in the process list and shell history; prefer --api-token-from-1password."`
	APITokenFrom1Password string        `name:"api-token-from-1password" placeholder:"op://..." help:"1Password secret reference for the provider API key. Without a key flag, provider.api_key from the config file applies, then OPENROUTER_API_KEY for openrouter."`
	SandboxImage          string        `name:"sandbox-image" placeholder:"REF" help:"Run the built-in tools of new sessions in an OpenShell sandbox from this worker image (see mise run image:worker), as the \"default\" environment. Environments in ~/.durable/config.json add images and network grants. Existing sessions keep their pin."`
	GitHostTokens         []string      `name:"git-host-token" sep:"none" placeholder:"KIND=SOURCE" help:"Token for a git host, as <kind>=<source>, where source is an op:// reference, gh or env:NAME. Only github is supported; without the flag, github reads git.hosts.github.token from the config file, then DURABLE_GITHUB_TOKEN. Needs --sandbox-image or an environment."`
	GitAuthor             string        `name:"git-author" placeholder:"NAME <EMAIL>" help:"The git user.name and user.email for commits in new sandboxed sessions. Default durable <durable@localhost>. Existing sessions keep their pin."`
	GitHubAPIURL          string        `name:"github-api-url" env:"DURABLE_GITHUB_API_URL" hidden:"" help:"GitHub API base URL, for tests."`
}

type cli struct {
	Serve       serveCmd         `cmd:"" default:"withargs" help:"Run the server."`
	Config      configCmd        `cmd:"" help:"Check the server config."`
	URL         urlCmd           `cmd:"" name:"url" help:"Print the browser link for the running server."`
	RotateToken rotateTokenCmd   `cmd:"" name:"rotate-token" help:"Replace the stored token. It takes effect when the server next starts."`
	SandboxDel  sandboxDeleteCmd `cmd:"" name:"sandbox-delete" help:"Delete a session's sandbox. The session keeps its pin, and its next open creates a fresh sandbox."`
}

type urlCmd struct{}

type rotateTokenCmd struct{}

const tokenFile = "server-token"

type serverInfo struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
}

func (c *serveCmd) overrides() config.Overrides {
	o := config.Overrides{
		Addr: c.Addr, IdleTimeout: c.IdleTimeout, OpenTimeout: c.OpenTimeout, Heartbeat: c.Heartbeat,
		Provider: c.Provider, Protocol: c.Protocol, Model: c.Model,
		SandboxImage: c.SandboxImage, GitHostFlags: c.GitHostTokens, GitAuthor: c.GitAuthor,
	}
	switch {
	case c.APIToken != "":
		o.APIKeyFlag = "--api-token"
	case c.APITokenFrom1Password != "":
		o.APIKeyFlag = "--api-token-from-1password"
	}
	return o
}

func (c *serveCmd) Run(ctx context.Context, logger *slog.Logger) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to find home directory: %w", err)
	}

	// Every config problem exits here, before the server locks the home or listens.
	settings, err := config.Load(home)
	if err != nil {
		return err
	}
	settings.Apply(c.overrides())
	err = settings.Validate()
	for _, w := range settings.Warnings {
		logger.Warn(w.Message, slog.String("field", w.Field))
	}
	if err != nil {
		return err
	}
	var shown []server.Setting
	for _, e := range settings.Entries() {
		shown = append(shown, server.Setting{Field: e.Field, Value: e.Value, Source: string(e.Source)})
	}
	var warnings []string
	for _, w := range settings.Warnings {
		warnings = append(warnings, w.Message)
	}
	gitSources := settings.GitHostSources()
	environments := settings.Environments
	gitAuthor := settings.GitIdentity()

	// Only the lock holder writes and removes server.json, so a second server can't break discovery.
	lock, err := lockHome(filepath.Join(home, ".durable"))
	if err != nil {
		return err
	}
	defer lock.Close()

	// Resolved once so a 1Password reference isn't read on every session open.
	apiToken, err := creds.ResolveAPIToken(ctx, c.APIToken, c.APITokenFrom1Password)
	if err != nil {
		return fmt.Errorf("failed to resolve API token: %w", err)
	}
	if apiToken == "" && settings.APIKey != "" {
		if apiToken, err = creds.Resolve(ctx, settings.APIKeySource()); err != nil {
			return fmt.Errorf("failed to resolve API token: %w", err)
		}
	}

	if err := checkGitHosts(ctx, logger, gitSources, c.GitHubAPIURL); err != nil {
		return err
	}

	token, err := loadOrCreateToken(filepath.Join(home, ".durable", tokenFile))
	if err != nil {
		return err
	}

	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", settings.Addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", settings.Addr, err)
	}
	addr := ln.Addr().String()
	if !isLoopback(ln.Addr()) {
		const msg = "listening on a non-loopback address: other machines can reach the API over plain HTTP, guarded only by the bearer token"
		logger.Warn(msg, slog.String("addr", addr))
		warnings = append(warnings, msg+" ("+addr+")")
	}
	infoPath := filepath.Join(home, ".durable", "server.json")
	if err := writeServerInfo(infoPath, serverInfo{Addr: addr, Token: token}); err != nil {
		_ = ln.Close()
		return err
	}
	defer os.Remove(infoPath)

	// Requests drain within the grace period, and streams end when the base context is cancelled.
	baseCtx, cancelBase := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelBase()
	// Closed after the registry, whose sessions stop their sandboxes through it.
	backend := &lazyBackend{tokens: gitTokenFuncs(gitSources)}
	defer backend.Close()
	sandboxCfg := &commands.SandboxConfig{Backend: sandbox.Shared(backend), Environments: environments, GitAuthor: gitAuthor}
	if len(environments) > 0 {
		sandboxCfg.GitHosts = map[string]githost.Host{github.Kind: github.New(c.GitHubAPIURL)}
		_, hasToken := gitSources[github.Kind]
		sandboxCfg.GitTokens = map[string]bool{github.Kind: hasToken}
	}
	// Runs outlive requests, so the registry's context is cancelled only after the server has drained.
	registry := server.NewRegistry(context.WithoutCancel(ctx), server.RegistryConfig{
		Home:        home,
		IdleTimeout: settings.IdleTimeout,
		OpenTimeout: settings.OpenTimeout,
		APIToken:    apiToken,
		Provider:    settings.Provider,
		Protocol:    settings.Protocol,
		Model:       settings.Model,
		Sandbox:     sandboxCfg,
		Logger:      logger,
	})
	defer registry.Close()
	draining := make(chan struct{})
	srv := &http.Server{
		Handler:           server.New(server.Config{Home: home, Token: token, Addr: addr, Logger: logger, Registry: registry, Heartbeat: settings.Heartbeat, Draining: draining, Settings: shown, Warnings: warnings}).Handler(),
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
	return writeFileAtomic(path, data, true)
}

// writeFileAtomic writes via a temp file; replace false fails with os.ErrExist if path exists.
func writeFileAtomic(path string, data []byte, replace bool) error {
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
	if !replace {
		// A link fails if path exists, so a complete file appears at most once.
		if err := os.Link(tmp.Name(), path); err != nil {
			return fmt.Errorf("failed to create %s: %w", path, err)
		}
		return nil
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("failed to replace %s: %w", path, err)
	}
	return nil
}

// lockHome takes an exclusive lock for the process lifetime; the kernel drops it on exit, so it is never stale.
func lockHome(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create %s: %w", dir, err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "server.lock"), os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // fixed name inside the durable directory
	if err != nil {
		return nil, fmt.Errorf("failed to open server lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another durable-server is running for this home; run durable-server url")
		}
		return nil, fmt.Errorf("failed to lock %s: %w", f.Name(), err)
	}
	return f, nil
}

// loadOrCreateToken keeps the token across restarts so a restart doesn't sign the browser out.
func loadOrCreateToken(path string) (string, error) {
	token := rand.Text()
	err := writeFileAtomic(path, []byte(token+"\n"), false)
	if err == nil {
		return token, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return "", err
	}
	st, err := os.Stat(path)
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
	token = strings.TrimSpace(string(data))
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
	fmt.Println(browserURL(info.Addr, info.Token))
	return nil
}

// isLoopback reports whether addr accepts connections only from this machine; a wildcard address does not.
func isLoopback(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}

// browserURL swaps a wildcard listen address for loopback, which a browser can open and the Host check allows.
func browserURL(addr, token string) string {
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
	}
	return "http://" + addr + "/#token=" + token
}

func (*rotateTokenCmd) Run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to find home directory: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(home, ".durable", tokenFile), []byte(rand.Text()+"\n"), true); err != nil {
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
		logger.Error("command failed", slog.String("command", k.Command()), slog.Any("error", err))
		os.Exit(1)
	}
}
