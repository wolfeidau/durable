package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wolfeidau/durable/internal/audit"
	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/sandbox/openshell"
	"github.com/wolfeidau/durable/internal/server"
)

const sandboxDeleteTimeout = 2 * time.Minute

// errGateway marks a failure to reach the gateway, which a retry may fix.
type errGateway struct{ err error }

func (e errGateway) Error() string { return e.err.Error() }
func (e errGateway) Unwrap() error { return e.err }

// lazyBackend connects to the OpenShell gateway on first use, so a server with no sandboxed sessions never contacts it.
type lazyBackend struct {
	// tokens resolve each git host's static token on demand.
	tokens map[string]func(ctx context.Context) (string, error)
	// apps resolve each git host's GitHub App credentials on demand.
	apps        map[string]func(ctx context.Context) (openshell.AppCredentials, error)
	appTokenURL string

	mu sync.Mutex
	l  *openshell.Launcher
}

var _ sandbox.Backend = (*lazyBackend)(nil)

// launcher retries a failed connect on the next call, because the gateway may come up later.
func (b *lazyBackend) launcher() (*openshell.Launcher, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.l == nil {
		l, err := openshell.New(openshell.Config{Tokens: b.tokens, Apps: b.apps, AppTokenURL: b.appTokenURL})
		if err != nil {
			return nil, errGateway{err}
		}
		b.l = l
	}
	return b.l, nil
}

func (b *lazyBackend) Ensure(ctx context.Context, s sandbox.Spec) error {
	l, err := b.launcher()
	if err != nil {
		return err
	}
	return l.Ensure(ctx, s)
}

func (b *lazyBackend) Stop(ctx context.Context, name string) error {
	l, err := b.launcher()
	if err != nil {
		return err
	}
	return l.Stop(ctx, name)
}

func (b *lazyBackend) Delete(ctx context.Context, name string) error {
	l, err := b.launcher()
	if err != nil {
		return err
	}
	return l.Delete(ctx, name)
}

func (b *lazyBackend) Connect(ctx context.Context, name, token string) (*sandbox.Client, io.Closer, error) {
	l, err := b.launcher()
	if err != nil {
		return nil, nil, err
	}
	return l.Connect(ctx, name, token)
}

// Decisions shares the backend's connection, so a client polling the gateway log doesn't dial it on every request.
func (b *lazyBackend) Decisions(ctx context.Context, name string, since time.Time) ([]openshell.Decision, error) {
	l, err := b.launcher()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to the gateway: %w", err)
	}
	return l.Decisions(ctx, name, since)
}

func (*lazyBackend) IsUnavailable(err error) bool {
	var ge errGateway
	return errors.As(err, &ge) || openshell.IsUnavailable(err)
}

func (b *lazyBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.l == nil {
		return nil
	}
	return b.l.Close()
}

type sandboxDeleteCmd struct {
	Session string `arg:"" help:"Session id, as shown in the session directory name."`
}

// Run keeps sandbox.json, so the next open creates a fresh, empty sandbox with the same pin.
func (c *sandboxDeleteCmd) Run(ctx context.Context) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to find home directory: %w", err)
	}
	dir, err := server.SessionDir(home, c.Session)
	if err != nil {
		return err
	}
	// The lock keeps this from racing a running server that has the sandbox open.
	lock, err := lockHome(filepath.Join(home, ".durable"))
	if err != nil {
		return fmt.Errorf("refusing to delete a sandbox while the server may be using it: %w", err)
	}
	defer lock.Close()

	pin, ok, err := sandbox.ReadPin(dir)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("session %s has no sandbox", c.Session)
	}
	b := &lazyBackend{}
	defer b.Close()
	dctx, cancel := context.WithTimeout(ctx, sandboxDeleteTimeout)
	defer cancel()
	if err := b.Delete(dctx, pin.Name); err != nil {
		return fmt.Errorf("failed to delete sandbox %s: %w", pin.Name, err)
	}
	if len(pin.Git) > 0 && pin.Git[0].CloneURL != "" {
		fmt.Printf("Deleted sandbox %s. The next open of session %s creates a fresh one and clones its branch again.\n", pin.Name, c.Session)
		return nil
	}
	fmt.Printf("Deleted sandbox %s. The next open of session %s creates a fresh, empty one.\n", pin.Name, c.Session)
	return nil
}

type sandboxAuditCmd struct {
	Session string `arg:"" help:"Session id, as shown in the session directory name."`
	// Zero means the default, so the command can tell an explicit --since.
	Since  time.Duration `help:"How far back to read the network decisions (default 24h)."`
	Denied bool          `help:"List only denied requests."`
}

const defaultAuditSince = 24 * time.Hour

func (c *sandboxAuditCmd) validate() error {
	if c.Since < 0 {
		return errors.New("--since must be positive")
	}
	return nil
}

// Run takes no lock, because it only reads the sandbox log.
func (c *sandboxAuditCmd) Run(ctx context.Context) error {
	if err := c.validate(); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to find home directory: %w", err)
	}
	dir, err := server.SessionDir(home, c.Session)
	if err != nil {
		return err
	}
	pin, ok, err := sandbox.ReadPin(dir)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("session %s has no sandbox", c.Session)
	}
	since := c.Since
	if since == 0 {
		since = defaultAuditSince
	}
	start := time.Now().Add(-since)
	gw := &lazyBackend{}
	defer gw.Close()
	res, err := audit.Read(ctx, audit.Query{Gateway: gw.Decisions, Sandbox: pin.Name, Since: start})
	if err != nil {
		return err
	}
	if res.Coverage.GatewayErr != nil {
		return res.Coverage.GatewayErr
	}
	return c.printDecisions(os.Stdout, res)
}

func (c *sandboxAuditCmd) printDecisions(w io.Writer, res audit.Result) error {
	cov := res.Coverage
	ds := make([]openshell.Decision, len(res.Events))
	for i, e := range res.Events {
		ds[i] = e.Decision
	}
	if len(ds) == 0 {
		fmt.Fprintln(w, "No network decisions for that period in the gateway log.")
		return nil
	}
	var allowed, denied int
	var shown []openshell.Decision
	for _, d := range ds {
		if d.Allowed {
			allowed++
		} else {
			denied++
		}
		if !c.Denied || !d.Allowed {
			shown = append(shown, d)
		}
	}
	if err := openshell.FormatDecisions(w, shown); err != nil {
		return fmt.Errorf("failed to write decisions: %w", err)
	}
	// The span can be shorter than --since, because the gateway keeps only recent log lines.
	fmt.Fprintf(w, "%d allowed, %d denied, from %s to %s\n", allowed, denied,
		ds[0].Time.Local().Format(time.DateTime), ds[len(ds)-1].Time.Local().Format(time.DateTime))
	fmt.Fprintln(w, "source: gateway log \u2014 only the gateway's most recent lines; older decisions are not shown")
	if !cov.GatewayStart.IsZero() {
		fmt.Fprintf(w, "note: the gateway log starts at %s, after the requested --since; earlier decisions are missing\n", localTime(cov.GatewayStart))
	}
	return nil
}

func localTime(t time.Time) string { return t.Local().Format(time.DateTime) }
