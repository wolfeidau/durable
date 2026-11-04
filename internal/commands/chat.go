package commands

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/mcp"
)

// The base prompt must stay fixed for the same cache and thinking reasons; pinned instructions are appended per session.
const systemPrompt = `You are a coding agent working in the user's current directory through the provided tools.
Read code before changing it. Make small, focused edits and verify them by running the project's tests or build when possible.
Prefer read_file, list_files and grep for exploration, and use bash for running commands.
If a tool result says a call was interrupted or declined, check the current state before retrying.
Be concise.`

// ChatCmd runs the interactive agent.
type ChatCmd struct {
	Provider  string        `help:"Model provider: anthropic or openrouter. Empty uses the session's pin, else anthropic."`
	Protocol  string        `help:"Wire protocol: anthropic-messages or openai-responses. Empty uses the session's pin, else the provider's default (anthropic: anthropic-messages, openrouter: openai-responses)."`
	Model     string        `help:"Model name. Required for a new openrouter session, which pins it. For anthropic, empty uses the built-in default."`
	Effort    string        `help:"Reasoning effort. Empty uses the built-in default on anthropic-messages and the model's own default on openai-responses."`
	Yes       bool          `help:"Run every tool that would ask, including edit_file, bash and MCP tools, without asking. Bash is not sandboxed."`
	NoContext bool          `name:"no-context" help:"Do not load AGENTS.md files or skills. Applies only when a session starts; resumed sessions keep their pinned instructions and skills."`
	Session   string        `default:"default" help:"Session name; sessions are per directory."`
	LeaseTTL  time.Duration `hidden:"" default:"0" help:"Session lease TTL (0 for the harness default)."`

	APIToken              string `name:"api-token" help:"API key for the provider. Visible in the process list and shell history; prefer --api-token-from-1password."`
	APITokenFrom1Password string `name:"api-token-from-1password" placeholder:"op://..." help:"1Password secret reference for the provider API key. Without a key flag, openrouter reads OPENROUTER_API_KEY."`
}

// Run opens the session, resumes unfinished work and reads prompts until EOF or interrupt.
func (c *ChatCmd) Run(ctx context.Context, _ *Globals) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get working directory: %w", err)
	}
	out := os.Stdout
	a, err := Assemble(ctx, AssembleConfig{
		CWD:                   cwd,
		Session:               c.Session,
		Provider:              c.Provider,
		Protocol:              c.Protocol,
		Model:                 c.Model,
		Effort:                c.Effort,
		APIToken:              c.APIToken,
		APITokenFrom1Password: c.APITokenFrom1Password,
		NoContext:             c.NoContext,
		LeaseTTL:              c.LeaseTTL,
		Yes:                   c.Yes,
		Interactive:           true,
		Out:                   out,
	})
	if err != nil {
		return err
	}
	defer a.Close()

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	lines := readLines(os.Stdin)

	a.printReport(out)

	shown := shownCalls{}
	opts := a.Options
	opts.OnDelta = printText(out)
	// The display wrapper stays outermost, so the call line prints before the tool, and any hooks, run.
	opts.Tools = withDisplay(opts.Tools, out, shown)
	if !c.Yes {
		opts.Ask = newApprover(lines, out, shown)
	}

	h, err := openWithNotice(ctx, a.Path, opts, sigs, func() {
		fmt.Fprintln(out, "waiting for session lease...")
	})
	if err != nil {
		return err
	}
	defer h.Close()

	r := &repl{h: h, skills: a.skills.Skills, lines: lines, sigs: sigs, out: out, piped: !term.IsTerminal(int(os.Stdin.Fd()))}
	return r.run(ctx)
}

// printReport shows what was pinned or discovered, in the order the CLI always has.
func (a *Assembled) printReport(out io.Writer) {
	for _, f := range a.instructions.Files {
		fmt.Fprintf(out, "[context] %s\n", f)
	}
	for _, w := range a.instructions.Warnings {
		fmt.Fprintf(out, "[context] warning: %s\n", w)
	}
	if a.instructions.Pinned {
		fmt.Fprintf(out, "[context] using pinned instructions (%d bytes); start a new --session to reload AGENTS.md\n", len(a.instructions.Text))
	}

	line, warning := routeLine(a.route)
	fmt.Fprintln(out, line)
	if warning != "" {
		fmt.Fprintln(out, warning)
	}
	printMCP(out, a.mcpServers, a.mcpWarnings, a.mcp)
	printSkills(out, a.skills)
	printHooks(out, a.hooks, a.hookWarnings, a.toolset)
	if a.CompactionNotice != "" {
		fmt.Fprintln(out, a.CompactionNotice)
	}
}

// printText streams model text to the terminal and ignores the other delta kinds.
func printText(out io.Writer) func(durable.Delta) {
	return func(d durable.Delta) {
		if d.Kind == durable.DeltaText {
			fmt.Fprint(out, d.Text)
		}
	}
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
