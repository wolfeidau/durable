package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/wolfeidau/durable/internal/audit"
	"github.com/wolfeidau/durable/internal/config"
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
	// Zero means the default, so the command can tell an explicit --since or --days.
	Since  time.Duration `help:"How far back to read the network decisions (default 24h). Not allowed with --daily."`
	Denied bool          `help:"List only denied requests. Not allowed with --daily or --raw."`
	Daily  bool          `help:"Print per-day totals from the telemetry store instead of the requests. Use --days, not --since."`
	Days   int           `help:"How many days --daily covers (default 30). Only allowed with --daily."`
	Raw    bool          `help:"Print the archived OTLP log records as JSON Lines, with notes on stderr. Needs the telemetry store; not allowed with --daily or --denied."`
}

const (
	defaultAuditSince = 24 * time.Hour
	defaultAuditDays  = 30
)

func (c *sandboxAuditCmd) validate() error {
	if c.Since < 0 {
		return errors.New("--since must be positive")
	}
	if c.Days < 0 {
		return errors.New("--days must be at least 1")
	}
	if c.Days != 0 && !c.Daily {
		return errors.New("--days applies only to --daily")
	}
	if c.Denied && c.Daily {
		return errors.New("--denied does not apply to --daily")
	}
	if c.Raw && c.Daily {
		return errors.New("--raw does not apply to --daily")
	}
	if c.Raw && c.Denied {
		return errors.New("--denied does not apply to --raw")
	}
	if c.Daily && c.Since != 0 {
		return errors.New("--since does not apply to --daily; use --days")
	}
	return nil
}

// Run takes no lock, because it only reads the telemetry store and the sandbox log.
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
	now := time.Now()
	dbPath := filepath.Join(home, ".durable", telemetryDB)
	_, statErr := os.Stat(dbPath)
	hasStore := statErr == nil
	if c.Daily && !hasStore {
		return errors.New("no telemetry store; enable telemetry.otlp_addr in config.json")
	}
	if c.Raw && !hasStore {
		return errors.New("--raw needs the telemetry store; enable telemetry.otlp_addr in config.json")
	}
	var store *telemetry.Store
	var retention time.Duration
	if hasStore {
		settings, err := config.Load(home)
		if err != nil {
			return err
		}
		retention = settings.TelemetryRetention
		if store, err = telemetry.OpenReadOnly(ctx, dbPath); err != nil {
			return fmt.Errorf("failed to open telemetry store: %w", err)
		}
		defer store.Close()
	}
	if c.Daily {
		return c.printDaily(ctx, store, pin.Name, now, retention)
	}

	since := c.Since
	if since == 0 {
		since = defaultAuditSince
	}
	start := now.Add(-since)
	if c.Raw {
		return printRaw(ctx, store, pin.Name, start, now, retention)
	}
	gw := &lazyBackend{}
	defer gw.Close()
	res, err := audit.Read(ctx, audit.Query{Store: store, Gateway: gw.Decisions, Sandbox: pin.Name, Since: start, Now: now, Retention: retention})
	if err != nil {
		return err
	}
	// With a store, an unreadable gateway only means the log adds nothing, so report it and still print the store notes.
	if store == nil && res.Coverage.GatewayErr != nil {
		return res.Coverage.GatewayErr
	}
	return c.printDecisions(os.Stdout, res, start)
}

func (c *sandboxAuditCmd) printDecisions(w io.Writer, res audit.Result, start time.Time) error {
	cov := res.Coverage
	ds := make([]openshell.Decision, len(res.Events))
	for i, e := range res.Events {
		ds[i] = e.Decision
	}
	if len(ds) == 0 {
		switch {
		case cov.GatewayErr != nil:
			fmt.Fprintf(w, "No network decisions in the telemetry store for that period; the gateway log could not be read: %v\n", cov.GatewayErr)
			printStoreNotes(w, cov, start)
		case cov.StoreAvailable:
			fmt.Fprintln(w, "No network decisions for that period; checked the telemetry store and the gateway log.")
			printStoreNotes(w, cov, start)
		default:
			fmt.Fprintln(w, "No network decisions for that period; checked the gateway log, because there is no telemetry store.")
		}
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
	if cov.Source == audit.SourceStore {
		fmt.Fprintln(w, "source: telemetry")
		printStoreNotes(w, cov, start)
		return nil
	}
	fmt.Fprintln(w, "source: gateway log \u2014 only the gateway's most recent lines; older decisions are not shown")
	if !cov.GatewayStart.IsZero() {
		fmt.Fprintf(w, "note: the gateway log starts at %s, after the requested --since; earlier decisions are missing\n", localTime(cov.GatewayStart))
	}
	if !cov.StoreAvailable {
		return nil
	}
	fmt.Fprintln(w, "note: the telemetry store has no decisions for this sandbox in this period; showing the gateway log instead")
	printStoreNotes(w, cov, start)
	return nil
}

// printRaw writes the archived records to stdout and every note to stderr, so stdout stays valid JSON Lines.
func printRaw(ctx context.Context, store *telemetry.Store, name string, start, now time.Time, retention time.Duration) error {
	ids, err := store.SandboxIDs(ctx, name)
	if err != nil {
		return err
	}
	events, err := store.RawEvents(ctx, name, ids, start)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, e := range events {
		buf.Reset()
		if err := json.Compact(&buf, e.Record); err != nil {
			return fmt.Errorf("failed to compact raw event at %s: %w", e.Time.Format(time.RFC3339), err)
		}
		buf.WriteByte('\n')
		if _, err := buf.WriteTo(os.Stdout); err != nil {
			return fmt.Errorf("failed to write raw event: %w", err)
		}
	}
	if len(events) == 0 {
		fmt.Fprintln(os.Stderr, "No raw events in the telemetry store for that period.")
	}
	var cov audit.Coverage
	if err := audit.ReadStoreCoverage(ctx, store, name, start, now, retention, &cov); err != nil {
		return err
	}
	printStoreNotes(os.Stderr, cov, start)
	return nil
}

func localTime(t time.Time) string { return t.Local().Format(time.DateTime) }

// printStoreNotes says what the telemetry store cannot show for a window starting at start.
func printStoreNotes(w io.Writer, cov audit.Coverage, start time.Time) {
	printDrops(w, cov.Drops, start)
	if !cov.HistoryStart.IsZero() {
		fmt.Fprintf(w, "note: telemetry history for this sandbox starts at %s; earlier decisions were not recorded\n", localTime(cov.HistoryStart))
	}
	if cov.Pruned {
		fmt.Fprintf(w, "note: raw events older than %s are pruned; use --daily for older totals\n", cov.Retention)
	}
}

func printDrops(w io.Writer, d audit.Drops, start time.Time) {
	if d.Own.Count > 0 {
		fmt.Fprintf(w, "warning: %d events from this sandbox were dropped before export since %s\n", d.Own.Count, localTime(d.Own.Since))
	}
	if d.GatewayWide.Count > 0 {
		fmt.Fprintf(w, "warning: %d events were dropped gateway-wide since %s; some may belong to this sandbox\n", d.GatewayWide.Count, localTime(d.GatewayWide.Since))
	}
	if d.Unknown.Count > 0 {
		fmt.Fprintf(w, "warning: %d events were dropped for sandboxes with no stored events since %s; some may belong to this sandbox\n", d.Unknown.Count, localTime(d.Unknown.Since))
	}
	if d.Unattributed > 0 {
		// Unattributed decisions are counted per UTC day, so the count covers the whole first day.
		fmt.Fprintf(w, "warning: %d network decisions on or after %s (UTC) had no sandbox or event id, so they are not attributed to any sandbox\n", d.Unattributed, start.UTC().Format(time.DateOnly))
	}
}

func (c *sandboxAuditCmd) printDaily(ctx context.Context, store *telemetry.Store, name string, now time.Time, retention time.Duration) error {
	days := c.Days
	if days == 0 {
		days = defaultAuditDays
	}
	start := now.Add(-time.Duration(days) * 24 * time.Hour)
	totals, err := store.Daily(ctx, name, start)
	if err != nil {
		return err
	}
	if len(totals) == 0 {
		fmt.Println("No network decisions in the telemetry store for that period.")
	} else {
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "DAY\tALLOWED\tDENIED\tTOP DENIED")
		for _, d := range totals {
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
	}
	if first, ok, err := store.FirstDay(ctx, name); err != nil {
		return err
	} else if ok && start.UTC().Format(time.DateOnly) < first {
		fmt.Printf("note: telemetry history for this sandbox starts on %s\n", first)
	}
	drops, err := audit.ReadDrops(ctx, store, name, start)
	if err != nil {
		return err
	}
	printDrops(os.Stdout, drops, start)
	if start.Before(now.Add(-retention)) {
		fmt.Printf("note: records of dropped events older than %s are pruned, so the warnings above may undercount for this range\n", retention)
	}
	return nil
}
