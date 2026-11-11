package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/compaction"
	"github.com/wolfeidau/durable/hooks"
	"github.com/wolfeidau/durable/internal/creds"
	"github.com/wolfeidau/durable/internal/githost"
	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/mcp"
	"github.com/wolfeidau/durable/skills"
	"github.com/wolfeidau/durable/tools"
)

// ErrNoSession is returned by Assemble with ExistingOnly when the session database doesn't exist.
var ErrNoSession = errors.New("session does not exist")

// ErrSandboxed is returned by Assemble for a sandboxed session when the caller has no sandbox support.
var ErrSandboxed = errors.New("this session runs in a sandbox; open it with durable-server")

// ErrEmptyNeedsSandbox is returned by Assemble for an empty or repo session that has no sandbox.
var ErrEmptyNeedsSandbox = errors.New("an empty or repo session needs a sandbox")

// SandboxConfig lets Assemble run a session's built-in tools in a sandbox.
type SandboxConfig struct {
	Backend sandbox.Backend
	// Image is the worker image for new sessions; empty keeps new sessions local. A session that already has a sandbox keeps its pin.
	Image string
	// GitHosts are the hosts a repo session can live on, by kind.
	GitHosts map[string]githost.Host
	// GitTokens is true for each host kind the server holds a token for; a repo on any other host is cloned anonymously.
	GitTokens map[string]bool
}

// AssembleConfig is everything Assemble needs to build a session's options without a terminal.
type AssembleConfig struct {
	CWD     string
	Session string
	// Workspace is WorkspaceEmpty or WorkspaceRepo for a session with no host directory, or empty for a directory session.
	Workspace string
	// SessionID is the empty or repo session's directory name, from EmptySessionID or RepoSessionID.
	SessionID string
	// Repo and Branch are what a repo session clones.
	Repo   githost.Repo
	Branch string
	// Home is where user config is read from; empty means the user's home directory.
	Home string

	Provider              string
	Protocol              string
	Model                 string
	Effort                string
	APIToken              string
	APITokenFrom1Password string

	NoContext bool
	LeaseTTL  time.Duration
	Yes       bool
	// Interactive is true when a terminal answers approvals; otherwise they are deferred.
	Interactive bool
	// Out receives compaction notices; nil discards them.
	Out io.Writer
	// ExistingOnly returns ErrNoSession instead of creating a session.
	ExistingOnly bool
	// Observer runs after the compaction observer, so it doesn't replace the compaction notices.
	Observer func(durable.Event)
	// Sandbox is nil when the caller can't run sandboxes, as for the CLI.
	Sandbox *SandboxConfig
	// Logger receives a failed sandbox stop on Close; nil discards it.
	Logger *slog.Logger
}

// Assembled is a session's options plus what the CLI reports about them.
type Assembled struct {
	Path string
	// Options has no Ask or OnDelta, and Tools are not wrapped for display.
	Options durable.Options
	// Close releases the MCP pool and stops the session's sandbox.
	Close func()

	instructions instructions
	route        route
	mcpServers   map[string]mcp.Server
	mcp          mcpTools
	mcpWarnings  []string
	skills       skillSet
	hooks        hooks.Config
	hookWarnings []string
	toolset      []durable.Tool
	// CompactionNotice is the line to show once the compaction window is known; empty when compaction is not client-side.
	CompactionNotice string
}

// Assemble resolves the session's pins and builds its durable.Options.
func Assemble(ctx context.Context, cfg AssembleConfig) (*Assembled, error) {
	out := cfg.Out
	if out == nil {
		out = io.Discard
	}
	path, err := cfg.sessionPath()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	_, statErr := os.Stat(path)
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("failed to check session database: %w", statErr)
	}
	existed := statErr == nil
	if cfg.ExistingOnly && !existed {
		return nil, ErrNoSession
	}
	if err := createSessionDir(dir, cfg.sessionMeta()); err != nil {
		return nil, err
	}
	useSandbox, err := cfg.checkSandboxed(dir, existed)
	if err != nil {
		return nil, err
	}

	// Pin after anything likely to fail, so a failed start doesn't freeze the text.
	home, err := cfg.homeDir()
	if err != nil {
		return nil, err
	}
	// Only the user's file: a repository must not be able to start commands.
	servers, mcpWarnings, err := mcp.LoadConfig(filepath.Join(home, ".durable", "mcp.json"))
	if err != nil {
		// A new session fails before anything is pinned; an existing one must still resume.
		if !existed {
			return nil, err
		}
		mcpWarnings = append(mcpWarnings, strings.ReplaceAll(err.Error(), "\n", "; "))
	}
	// A policy the user configured must not be skipped, so unlike MCP an existing session refuses too.
	hookCfg, hookWarnings, err := hooks.LoadConfig(filepath.Join(home, ".durable", "hooks.json"))
	if err != nil {
		return nil, err
	}
	rt, spec, key, err := cfg.resolveRoute(ctx, dir, existed)
	if err != nil {
		return nil, err
	}

	// After the route, so a misconfigured provider fails before a sandbox is created, and before the other pins.
	toolset, client, releaseSandbox, err := cfg.openTools(ctx, dir, useSandbox, !existed, servers)
	if err != nil {
		return nil, err
	}
	pinned, err := pinInstructions(dir, existed, cfg.NoContext, func() (instructions, error) {
		return discoverInstructions(cfg.CWD, home)
	})
	if err != nil {
		releaseSandbox(!existed)
		return nil, err
	}

	poolOpts := mcp.Options{Root: cfg.CWD, LogDir: dir}
	if useSandbox {
		// The servers run beside the worker, so nothing starts on the host.
		poolOpts.Dial = sandboxDial(client, dir)
	}
	pool := mcp.NewPool(servers, poolOpts)
	mcpPinned, err := pinMCPTools(ctx, dir, existed, pool.Discover)
	if err != nil {
		_ = pool.Close()
		releaseSandbox(!existed)
		return nil, err
	}
	toolset = append(toolset, pool.Tools(mcpPinned.Tools)...)
	skillsPinned, err := pinSkills(dir, existed, cfg.NoContext, func() ([]skills.Skill, []string, error) {
		srcs, err := skillSources(cfg.CWD, home)
		if err != nil {
			return nil, nil, err
		}
		found, warns := skills.Discover(srcs)
		return found, warns, nil
	})
	if err != nil {
		_ = pool.Close()
		releaseSandbox(!existed)
		return nil, err
	}
	// Added before the display wrapper and the gate so the read-only tool runs without prompting.
	if t := skills.Tool(skillsPinned.Skills); t != nil {
		toolset = append(toolset, t)
	}

	sessionID := filepath.Base(dir)
	opts := durable.Options{
		Model: protocolModels[rt.Protocol](rt, spec, modelArgs{
			Key:       key,
			System:    buildPrompt(pinned.Text),
			Effort:    cfg.Effort,
			SessionID: sessionID,
			OnCompact: func() { fmt.Fprintln(out, "\n[compacting history]") },
		}),
		Tools:    toolset,
		LeaseTTL: cfg.LeaseTTL,
	}
	notice := addCompaction(ctx, out, &opts, rt, spec, modelArgs{Key: key, System: compaction.SystemPrompt, Effort: cfg.Effort, SessionID: sessionID})
	opts.Observer = chainObservers(opts.Observer, cfg.Observer)
	var base durable.ToolGate
	if !cfg.Yes {
		base = durable.ReadOnlyGate(toolset)
		opts.Gate = base
	}
	if !hookCfg.Empty() {
		// base comes from the unwrapped tools, so the display wrapper can stay outermost.
		runner := newHookRunner(hookCfg, cfg.CWD, dir, cfg.Yes)
		opts.Gate = runner.Gate(base)
		opts.Tools = runner.Wrap(toolset)
	}
	// With no gate the harness parks every call, which would defeat Yes.
	opts.DeferApprovals = !cfg.Interactive && opts.Gate != nil

	return &Assembled{
		Path:    path,
		Options: opts,
		Close: func() {
			_ = pool.Close()
			releaseSandbox(false)
		},
		instructions:     pinned,
		route:            rt,
		mcpServers:       servers,
		mcp:              mcpPinned,
		mcpWarnings:      mcpWarnings,
		skills:           skillsPinned,
		hooks:            hookCfg,
		hookWarnings:     hookWarnings,
		toolset:          toolset,
		CompactionNotice: notice,
	}, nil
}

func (c AssembleConfig) sessionMeta() SessionMeta {
	meta := SessionMeta{CWD: c.CWD, Name: c.Session, Workspace: c.Workspace, CreatedAt: time.Now().UTC()}
	if c.Workspace == WorkspaceRepo {
		meta.Repo, meta.Branch = c.Repo.String(), c.Branch
	}
	return meta
}

func (c AssembleConfig) sessionPath() (string, error) {
	if c.Workspace == "" {
		return SessionPath(c.CWD, c.Session)
	}
	home, err := c.homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".durable", "sessions", c.SessionID, "session.sqlite"), nil
}

func (c AssembleConfig) homeDir() (string, error) {
	if c.Home != "" {
		return c.Home, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to find home directory: %w", err)
	}
	return home, nil
}

// checkSandboxed reports whether the session's built-in tools run in a sandbox: it has one, or it is new and the caller
// asks for one. It refuses a sandboxed session when the caller can't run one.
func (c AssembleConfig) checkSandboxed(dir string, existed bool) (bool, error) {
	_, ok, err := sandbox.ReadPin(dir)
	if err != nil {
		return false, err
	}
	if ok && c.Sandbox == nil {
		return false, ErrSandboxed
	}
	sandboxed := ok || c.Sandbox != nil && !existed && c.Sandbox.Image != ""
	if c.Workspace != "" && !sandboxed {
		return false, ErrEmptyNeedsSandbox
	}
	return sandboxed, nil
}

// openTools returns the built-in tools, and the worker's client when sandboxed: from the session's sandbox when it has one or this new session asks for one,
// else local. The sandbox's tools equal tools.All in names, order and specs (sandbox.Open enforces it), so the prompt
// prefix and tool order don't change with where they run.
func (c AssembleConfig) openTools(ctx context.Context, dir string, useSandbox, fresh bool, servers map[string]mcp.Server) ([]durable.Tool, *sandbox.Client, func(discard bool), error) {
	if !useSandbox {
		toolset, err := tools.All(c.CWD, dir)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to build tools: %w", err)
		}
		return toolset, nil, func(bool) {}, nil
	}
	// Pinned before the sandbox is created, so a failed create leaves its token on disk for the retry.
	pin, ok, err := sandbox.ReadPin(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	if !ok {
		var access []sandbox.GitAccess
		if c.Workspace == WorkspaceRepo {
			a, err := c.repoAccess()
			if err != nil {
				return nil, nil, nil, err
			}
			access = []sandbox.GitAccess{a}
		}
		// The access is fixed here, so a reopen keeps it even if the server's tokens changed.
		if pin, err = sandbox.WritePinGit(dir, c.Sandbox.Image, mcpEgress(servers), access); err != nil {
			return nil, nil, nil, err
		}
	}
	toolset, client, release, err := sandbox.Open(ctx, c.Sandbox.Backend, pin, fresh)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to open sandbox: %w", err)
	}
	releaseSandbox := func(discard bool) {
		if err := release(discard); err != nil && c.Logger != nil {
			c.Logger.ErrorContext(ctx, "failed to release sandbox", slog.String("sandbox", pin.Name), slog.Any("error", err))
		}
	}
	if c.Workspace == WorkspaceRepo {
		if len(pin.Git) == 0 {
			releaseSandbox(fresh)
			return nil, nil, nil, errors.New("repo session pin has no repository")
		}
		// A new sandbox is discarded so the retry clones afresh; the pin stays.
		if err := ensureClone(ctx, client, pin.Git[0].CloneURL, c.Branch); err != nil {
			releaseSandbox(fresh)
			return nil, nil, nil, err
		}
	}
	return toolset, client, releaseSandbox, nil
}

func chainObservers(first, second func(durable.Event)) func(durable.Event) {
	switch {
	case first == nil:
		return second
	case second == nil:
		return first
	}
	return func(e durable.Event) {
		first(e)
		second(e)
	}
}

// addCompaction summarises with the session's own model, and the harness gives the Compactor no delta sink, so the summary stays off the terminal.
// It returns the window notice for the caller to print at the right point.
func addCompaction(ctx context.Context, out io.Writer, opts *durable.Options, rt route, spec providerSpec, args modelArgs) string {
	// The direct anthropic route keeps server-side compaction.
	if rt.Provider != providerOpenRouter {
		return ""
	}
	// The window doesn't feed the prompt prefix, so it isn't pinned and a failed fetch doesn't block the session.
	var notice string
	window, err := fetchContextWindow(ctx, &http.Client{}, baseURL(spec), args.Key, rt.Model)
	if err != nil {
		notice = fmt.Sprintf("[compaction] warning: %s; compaction runs only if the provider reports an overflow", err)
		window = 0
	} else {
		notice = fmt.Sprintf("[compaction] context window %d tokens", window)
	}
	opts.Compactor = compaction.New(compaction.Config{Model: protocolModels[rt.Protocol](rt, spec, args), ContextWindow: window})
	opts.Observer = func(e durable.Event) {
		switch e.Kind {
		case durable.EventCompacted:
			fmt.Fprintln(out, "\n[compacting history]")
		case durable.EventCompactionFailed:
			fmt.Fprintf(out, "\n[compaction] warning: %s\n", e.Error)
		default:
		}
	}
	return notice
}

// resolveRoute validates the route and its key before any pin is written, so a failed start doesn't freeze the session.
func (c AssembleConfig) resolveRoute(ctx context.Context, dir string, existed bool) (route, providerSpec, string, error) {
	flagToken := sync.OnceValues(func() (string, error) {
		tok, err := creds.ResolveAPIToken(ctx, c.APIToken, c.APITokenFrom1Password)
		if err != nil {
			return "", fmt.Errorf("failed to resolve API token: %w", err)
		}
		return tok, nil
	})
	var key string
	rt, _, err := pinRoute(dir, existed, routeFlags{Provider: c.Provider, Protocol: c.Protocol, Model: c.Model}, func(r route) error {
		s, err := r.spec()
		if err != nil {
			return err
		}
		tok, err := flagToken()
		if err != nil {
			return err
		}
		key, err = resolveKey(tok, r, s)
		return err
	})
	if err != nil {
		return route{}, providerSpec{}, "", err
	}
	if rt.Provider == providerAnthropic {
		rt.Model = c.Model
	}
	spec, err := rt.spec()
	if err != nil {
		return route{}, providerSpec{}, "", err
	}
	return rt, spec, key, nil
}
