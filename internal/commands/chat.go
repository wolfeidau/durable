package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"golang.org/x/term"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/claude"
	"github.com/wolfeidau/durable/internal/creds"
	"github.com/wolfeidau/durable/mcp"
	"github.com/wolfeidau/durable/skills"
	"github.com/wolfeidau/durable/tools"
)

// The base prompt must stay fixed for the same cache and thinking reasons; pinned instructions are appended per session.
const systemPrompt = `You are a coding agent working in the user's current directory through the provided tools.
Read code before changing it. Make small, focused edits and verify them by running the project's tests or build when possible.
Prefer read_file, list_files and grep for exploration, and use bash for running commands.
If a tool result says a call was interrupted or declined, check the current state before retrying.
Be concise.`

// ChatCmd runs the interactive agent.
type ChatCmd struct {
	Model     string        `help:"Claude model; empty uses the built-in default."`
	Effort    string        `help:"Reasoning effort; empty uses the built-in default."`
	Yes       bool          `help:"Run edit_file and bash without asking. Bash is not sandboxed."`
	NoContext bool          `name:"no-context" help:"Do not load AGENTS.md files or skills. Applies only when a session starts; resumed sessions keep their pinned instructions and skills."`
	Session   string        `default:"default" help:"Session name; sessions are per directory."`
	LeaseTTL  time.Duration `hidden:"" default:"0" help:"Session lease TTL (0 for the harness default)."`

	APIToken              string `name:"api-token" help:"Claude API key. Visible in the process list and shell history; prefer --api-token-from-1password."`
	APITokenFrom1Password string `name:"api-token-from-1password" placeholder:"op://..." help:"1Password secret reference for the Claude API key."`
}

// Run opens the session, resumes unfinished work and reads prompts until EOF or interrupt.
func (c *ChatCmd) Run(ctx context.Context, _ *Globals) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get working directory: %w", err)
	}
	path, err := SessionPath(cwd, c.Session)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}
	toolset, err := tools.All(cwd, filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("failed to build tools: %w", err)
	}
	token, err := creds.ResolveAPIToken(ctx, c.APIToken, c.APITokenFrom1Password)
	if err != nil {
		return fmt.Errorf("failed to resolve API token: %w", err)
	}
	var clientOpts []option.RequestOption
	if token != "" {
		clientOpts = append(clientOpts, option.WithAPIKey(token))
	}

	// Pin after anything likely to fail, so a failed start doesn't freeze the text.
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to find home directory: %w", err)
	}
	// Check before openWithNotice creates the database.
	_, statErr := os.Stat(path)
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("failed to check session database: %w", statErr)
	}
	// Only the user's file: a repository must not be able to start commands.
	servers, mcpWarnings, err := mcp.LoadConfig(filepath.Join(home, ".durable", "mcp.json"))
	if err != nil {
		// A new session fails before anything is pinned; an existing one must still resume.
		if statErr != nil {
			return err
		}
		mcpWarnings = append(mcpWarnings, strings.ReplaceAll(err.Error(), "\n", "; "))
	}
	pinned, err := pinInstructions(filepath.Dir(path), statErr == nil, c.NoContext, func() (instructions, error) {
		return discoverInstructions(cwd, home)
	})
	if err != nil {
		return err
	}

	pool := mcp.NewPool(servers, mcp.Options{Root: cwd, LogDir: filepath.Dir(path)})
	defer pool.Close()
	mcpPinned, err := pinMCPTools(ctx, filepath.Dir(path), statErr == nil, pool.Discover)
	if err != nil {
		return err
	}
	toolset = append(toolset, pool.Tools(mcpPinned.Tools)...)
	skillsPinned, err := pinSkills(filepath.Dir(path), statErr == nil, c.NoContext, func() ([]skills.Skill, []string, error) {
		srcs, err := skillSources(cwd, home)
		if err != nil {
			return nil, nil, err
		}
		found, warns := skills.Discover(srcs)
		return found, warns, nil
	})
	if err != nil {
		return err
	}
	// Added before withDisplay and the gate so the read-only tool runs without prompting.
	if t := skills.Tool(skillsPinned.Skills); t != nil {
		toolset = append(toolset, t)
	}

	out := os.Stdout
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	lines := readLines(os.Stdin)

	for _, f := range pinned.Files {
		fmt.Fprintf(out, "[context] %s\n", f)
	}
	for _, w := range pinned.Warnings {
		fmt.Fprintf(out, "[context] warning: %s\n", w)
	}
	if pinned.Pinned {
		fmt.Fprintf(out, "[context] using pinned instructions (%d bytes); start a new --session to reload AGENTS.md\n", len(pinned.Text))
	}

	printMCP(out, servers, mcpWarnings, mcpPinned)
	printSkills(out, skillsPinned)

	shown := shownCalls{}
	opts := durable.Options{
		Model: claude.New(anthropic.NewClient(clientOpts...), claude.Config{
			Model:     c.Model,
			System:    buildPrompt(pinned.Text),
			Effort:    c.Effort,
			OnText:    func(s string) { fmt.Fprint(out, s) },
			OnCompact: func() { fmt.Fprintln(out, "\n[compacting history]") },
		}),
		Tools:    withDisplay(toolset, out, shown),
		LeaseTTL: c.LeaseTTL,
	}
	if !c.Yes {
		opts.Gate = durable.ReadOnlyGate(toolset)
		opts.Ask = newApprover(lines, out, shown)
	}

	h, err := openWithNotice(ctx, path, opts, sigs, func() {
		fmt.Fprintln(out, "waiting for session lease...")
	})
	if err != nil {
		return err
	}
	defer h.Close()

	r := &repl{h: h, skills: skillsPinned.Skills, lines: lines, sigs: sigs, out: out, piped: !term.IsTerminal(int(os.Stdin.Fd()))}
	return r.run(ctx)
}

func printMCP(out io.Writer, servers map[string]mcp.Server, configWarnings []string, p mcpTools) {
	for _, w := range configWarnings {
		fmt.Fprintf(out, "[mcp] warning: %s\n", w)
	}
	for _, w := range p.Warnings {
		fmt.Fprintf(out, "[mcp] warning: %s\n", w)
	}
	switch {
	case len(p.Tools) > 0 && p.Pinned:
		fmt.Fprintf(out, "[mcp] using %d pinned tools; start a new --session to reload MCP servers\n", len(p.Tools))
	case len(p.Tools) > 0:
		counts := map[string]int{}
		for _, t := range p.Tools {
			counts[t.Server]++
		}
		for _, name := range slices.Sorted(maps.Keys(counts)) {
			fmt.Fprintf(out, "[mcp] %s: %d tools\n", name, counts[name])
		}
	case p.Pinned && len(servers) > 0:
		fmt.Fprintln(out, "[mcp] this session has no MCP tools pinned, so configured servers are not used; start a new --session to use them")
	}
}

func printSkills(out io.Writer, p skillSet) {
	for _, w := range p.Warnings {
		fmt.Fprintf(out, "[skills] warning: %s\n", w)
	}
	switch {
	case len(p.Skills) == 0:
	case p.Pinned:
		fmt.Fprintf(out, "[skills] using %d pinned skills; start a new --session to reload skills\n", len(p.Skills))
	default:
		names := make([]string, len(p.Skills))
		for i, s := range p.Skills {
			names[i] = s.Name
		}
		fmt.Fprintf(out, "[skills] %d skills: %s\n", len(names), strings.Join(names, ", "))
	}
}

// openWithNotice lets Ctrl-C abandon a lease wait and calls notice if Open is slow.
func openWithNotice(ctx context.Context, path string, opts durable.Options, sigs <-chan os.Signal, notice func()) (*durable.Harness, error) {
	octx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-stop:
		}
	}()
	timer := time.AfterFunc(time.Second, notice)
	defer timer.Stop()

	h, err := durable.Open(octx, path, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to open session: %w", err)
	}
	return h, nil
}
