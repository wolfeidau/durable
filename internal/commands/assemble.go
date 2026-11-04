package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	"github.com/wolfeidau/durable/mcp"
	"github.com/wolfeidau/durable/skills"
	"github.com/wolfeidau/durable/tools"
)

// ErrNoSession is returned by Assemble with ExistingOnly when the session database doesn't exist.
var ErrNoSession = errors.New("session does not exist")

// AssembleConfig is everything Assemble needs to build a session's options without a terminal.
type AssembleConfig struct {
	CWD     string
	Session string

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
}

// Assembled is a session's options plus what the CLI reports about them.
type Assembled struct {
	Path string
	// Options has no Ask or OnDelta, and Tools are not wrapped for display.
	Options durable.Options
	// Close releases the MCP pool.
	Close func()

	Instructions instructions
	Route        route
	MCPServers   map[string]mcp.Server
	MCP          mcpTools
	MCPWarnings  []string
	Skills       skillSet
	Hooks        hooks.Config
	HookWarnings []string
	Toolset      []durable.Tool
	// CompactionNotice is the line to show once the compaction window is known; empty when compaction is not client-side.
	CompactionNotice string
}

// Assemble resolves the session's pins and builds its durable.Options.
func Assemble(ctx context.Context, cfg AssembleConfig) (*Assembled, error) {
	out := cfg.Out
	if out == nil {
		out = io.Discard
	}
	path, err := SessionPath(cfg.CWD, cfg.Session)
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
	if err := createSessionDir(dir, SessionMeta{CWD: cfg.CWD, Name: cfg.Session, CreatedAt: time.Now().UTC()}); err != nil {
		return nil, err
	}
	toolset, err := tools.All(cfg.CWD, dir)
	if err != nil {
		return nil, fmt.Errorf("failed to build tools: %w", err)
	}

	// Pin after anything likely to fail, so a failed start doesn't freeze the text.
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to find home directory: %w", err)
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
	pinned, err := pinInstructions(dir, existed, cfg.NoContext, func() (instructions, error) {
		return discoverInstructions(cfg.CWD, home)
	})
	if err != nil {
		return nil, err
	}

	pool := mcp.NewPool(servers, mcp.Options{Root: cfg.CWD, LogDir: dir})
	mcpPinned, err := pinMCPTools(ctx, dir, existed, pool.Discover)
	if err != nil {
		_ = pool.Close()
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
		Tools:          toolset,
		LeaseTTL:       cfg.LeaseTTL,
		DeferApprovals: !cfg.Interactive,
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

	return &Assembled{
		Path:             path,
		Options:          opts,
		Close:            func() { _ = pool.Close() },
		Instructions:     pinned,
		Route:            rt,
		MCPServers:       servers,
		MCP:              mcpPinned,
		MCPWarnings:      mcpWarnings,
		Skills:           skillsPinned,
		Hooks:            hookCfg,
		HookWarnings:     hookWarnings,
		Toolset:          toolset,
		CompactionNotice: notice,
	}, nil
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
