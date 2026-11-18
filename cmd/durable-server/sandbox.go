package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/sandbox/openshell"
	"github.com/wolfeidau/durable/internal/server"
	"github.com/wolfeidau/durable/internal/telemetry"
)

const sandboxDeleteTimeout = 2 * time.Minute

// errGateway marks a failure to reach the gateway, which a retry may fix.
type errGateway struct{ err error }

func (e errGateway) Error() string { return e.err.Error() }
func (e errGateway) Unwrap() error { return e.err }

// lazyBackend connects to the OpenShell gateway on first use, so a server with no sandboxed sessions never contacts it.
type lazyBackend struct {
	// tokens resolve each git host's token on demand.
	tokens map[string]func(ctx context.Context) (string, error)

	mu sync.Mutex
	l  *openshell.Launcher
}

var _ sandbox.Backend = (*lazyBackend)(nil)

// launcher retries a failed connect on the next call, because the gateway may come up later.
func (b *lazyBackend) launcher() (*openshell.Launcher, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.l == nil {
		l, err := openshell.New(openshell.Config{Tokens: b.tokens})
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
	Session string        `arg:"" help:"Session id, as shown in the session directory name."`
	Since   time.Duration `default:"24h" help:"How far back to read the network decisions."`
	Denied  bool          `help:"List only denied requests."`
	Daily   bool          `help:"Print per-day totals from the telemetry store instead of the requests."`
	Days    int           `default:"30" help:"How many days --daily covers."`
}

// Run takes no lock, because it only reads the telemetry store and the sandbox log.
func (c *sandboxAuditCmd) Run(ctx context.Context) error {
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
	now := time.Now()
	dbPath := filepath.Join(home, ".durable", telemetryDB)
	_, statErr := os.Stat(dbPath)
	hasStore := statErr == nil
	if c.Daily && !hasStore {
		return errors.New("no telemetry store; enable telemetry.otlp_addr in config.json")
	}
	var store *telemetry.Store
	if hasStore {
		if store, err = telemetry.OpenReadOnly(ctx, dbPath); err != nil {
			return fmt.Errorf("failed to open telemetry store: %w", err)
		}
		defer store.Close()
	}
	if c.Daily {
		return c.printDaily(ctx, store, pin.Name, now)
	}

	since := now.Add(-c.Since)
	ds, source, store, err := auditDecisions(ctx, store, pin.Name, since)
	if err != nil {
		return err
	}
	if len(ds) == 0 {
		fmt.Println("No network decisions in the sandbox log for that period.")
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
	if err := openshell.FormatDecisions(os.Stdout, shown); err != nil {
		return fmt.Errorf("failed to write decisions: %w", err)
	}
	// The span can be shorter than --since, because the gateway keeps only recent log lines.
	fmt.Printf("%d allowed, %d denied, from %s to %s\n", allowed, denied,
		ds[0].Time.Local().Format(time.DateTime), ds[len(ds)-1].Time.Local().Format(time.DateTime))
	fmt.Println("source: " + source)
	if store != nil {
		return printGaps(ctx, store, pin.Name, since)
	}
	return nil
}

// auditDecisions prefers the telemetry store and falls back to the gateway's log; the returned store is nil when it was not the source.
func auditDecisions(ctx context.Context, store *telemetry.Store, name string, since time.Time) ([]openshell.Decision, string, *telemetry.Store, error) {
	if store != nil {
		ds, err := store.Decisions(ctx, name, since)
		if err != nil || len(ds) > 0 {
			return ds, "telemetry", store, err
		}
	}
	ds, err := gatewayDecisions(ctx, name, since)
	return ds, "gateway log (recent lines only)", nil, err
}

func gatewayDecisions(ctx context.Context, name string, since time.Time) ([]openshell.Decision, error) {
	l, err := openshell.New(openshell.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to the gateway: %w", err)
	}
	defer l.Close()
	return l.Decisions(ctx, name, since)
}

func printGaps(ctx context.Context, store *telemetry.Store, name string, since time.Time) error {
	ids, err := store.SandboxIDs(ctx, name)
	if err != nil {
		return err
	}
	gaps, err := store.Gaps(ctx, ids, since)
	if err != nil {
		return err
	}
	if len(gaps) == 0 {
		return nil
	}
	var dropped int64
	earliest := gaps[0].Since
	for _, g := range gaps {
		dropped += g.Dropped
		if g.Since.Before(earliest) {
			earliest = g.Since
		}
	}
	fmt.Printf("warning: %d events dropped since %s\n", dropped, earliest.Local().Format(time.DateTime))
	return nil
}

func (c *sandboxAuditCmd) printDaily(ctx context.Context, store *telemetry.Store, name string, now time.Time) error {
	days, err := store.Daily(ctx, name, now.Add(-time.Duration(c.Days)*24*time.Hour))
	if err != nil {
		return err
	}
	if len(days) == 0 {
		fmt.Println("No network decisions in the telemetry store for that period.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "DAY\tALLOWED\tDENIED\tTOP DENIED")
	for _, d := range days {
		hosts := make([]string, len(d.TopDenied))
		for i, h := range d.TopDenied {
			hosts[i] = fmt.Sprintf("%s (%d)", h.Host, h.Count)
		}
		top := "-"
		if len(hosts) > 0 {
			top = strings.Join(hosts, ", ")
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", d.Day, d.Allowed, d.Denied, top)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("failed to write daily totals: %w", err)
	}
	return nil
}
