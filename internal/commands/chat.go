package commands

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"golang.org/x/term"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/claude"
	"github.com/wolfeidau/durable/internal/creds"
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
	NoContext bool          `name:"no-context" help:"Do not load AGENTS.md files. Applies only when a session starts; resumed sessions keep their pinned instructions."`
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
	pinned, err := pinInstructions(filepath.Dir(path), statErr == nil, c.NoContext, func() (instructions, error) {
		return discoverInstructions(cwd, home)
	})
	if err != nil {
		return err
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

	r := &repl{h: h, lines: lines, sigs: sigs, out: out, piped: !term.IsTerminal(int(os.Stdin.Fd()))}
	return r.run(ctx)
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
