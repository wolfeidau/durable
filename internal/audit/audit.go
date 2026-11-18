// Package audit reads a sandbox's network decisions and says what the result cannot show.
package audit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wolfeidau/durable/internal/sandbox/openshell"
	"github.com/wolfeidau/durable/internal/telemetry"
)

// Gateway reads the sandbox's decisions from the gateway log, which keeps only recent lines.
type Gateway func(ctx context.Context, name string, since time.Time) ([]openshell.Decision, error)

// Source is where the decisions came from.
type Source int

const (
	// SourceStore is the telemetry store.
	SourceStore Source = iota
	// SourceGateway is the gateway's log, a bounded buffer.
	SourceGateway
)

// Drop is events lost before or during receipt.
type Drop struct {
	Count int64
	Since time.Time // the earliest gap's start; zero when Count is zero
}

// Drops is what the telemetry store knows was lost, from the window start.
type Drops struct {
	// Own was dropped by the exporter for this sandbox.
	Own Drop
	// GatewayWide was dropped for the whole gateway, so some may belong to this sandbox.
	GatewayWide Drop
	// Unknown was dropped for sandbox IDs the store has no named event for, so some may belong to this sandbox.
	Unknown Drop
	// Unattributed counts decisions with no sandbox or event id, per UTC day from the window start's day.
	Unattributed int64
}

// Coverage says what the decisions do not show.
type Coverage struct {
	Source Source
	// StoreAvailable is false when there is no telemetry store, so every other store field is empty.
	StoreAvailable bool
	// StoreEmpty is set when the store had no decisions in the window and the gateway log was read instead.
	StoreEmpty bool
	// GatewayErr is why the gateway log could not be read; it is set only when the log was needed.
	GatewayErr error
	// GatewayStart is the first gateway decision when it is later than the window start, so earlier ones are missing.
	GatewayStart time.Time
	// HistoryStart is the sandbox's earliest stored decision when it is later than the window start.
	HistoryStart time.Time
	Drops        Drops
	Retention    time.Duration
	// Pruned is set when the window starts before the retention cutoff, so older rows are gone.
	Pruned bool
}

// Query selects the decisions to read.
type Query struct {
	Store     *telemetry.Store // nil when telemetry is off
	Gateway   Gateway          // nil when no gateway reader is configured
	Sandbox   string
	Since     time.Time
	Now       time.Time
	Retention time.Duration
	// After is a NextCursor from an earlier Result; empty reads the newest Limit events in the window.
	After string
	// Limit caps the events returned; zero or less returns them all.
	Limit int
}

// Event is one decision with the id that orders it. The id is the store's uid, or for the gateway log a hash of the
// decision's content, because the log has no uid.
type Event struct {
	ID string
	openshell.Decision
}

// Result is the events, oldest first, and their coverage.
type Result struct {
	Events   []Event
	Coverage Coverage
	// NextCursor resumes after the last event; it echoes Query.After when there are none.
	NextCursor string
	// Truncated means Limit left events out: older ones in the window when Query.After is empty, newer ones otherwise.
	Truncated bool
}

var errNoGateway = errors.New("no gateway log reader is configured")

// ErrBadCursor marks a cursor that is malformed or that this server cannot resume.
var ErrBadCursor = errors.New("invalid cursor")

// Read prefers the telemetry store and falls back to the gateway log when the store has none.
// A gateway failure is reported in Coverage.GatewayErr, not returned, because the store may still add coverage.
// A cursor stays with the source that issued it, so a store cursor never falls back to the log.
func Read(ctx context.Context, q Query) (Result, error) {
	cur, err := parseCursor(q.After)
	if err != nil {
		return Result{}, err
	}
	if cur != nil && cur.source == SourceStore && q.Store == nil {
		return Result{}, fmt.Errorf("%w: it came from the telemetry store, which is not available", ErrBadCursor)
	}
	res := Result{NextCursor: q.After, Coverage: Coverage{Source: SourceStore, StoreAvailable: q.Store != nil}}
	if q.Store != nil && (cur == nil || cur.source == SourceStore) {
		if err := q.readStore(ctx, cur, &res); err != nil {
			return Result{}, err
		}
	}
	if len(res.Events) == 0 && (cur == nil || cur.source == SourceGateway) {
		res.Coverage.Source = SourceGateway
		res.Coverage.StoreEmpty = q.Store != nil && cur == nil
		q.readGateway(ctx, cur, &res)
	}
	if q.Store == nil {
		return res, nil
	}
	if err := ReadStoreCoverage(ctx, q.Store, q.Sandbox, q.Since, q.Now, q.Retention, &res.Coverage); err != nil {
		return Result{}, err
	}
	return res, nil
}

func (q Query) readStore(ctx context.Context, cur *cursor, res *Result) error {
	var after *telemetry.Position
	if cur != nil {
		after = &cur.pos
	}
	rows, more, err := q.Store.DecisionsPage(ctx, q.Sandbox, q.Since, after, q.Limit)
	if err != nil {
		return err
	}
	for _, r := range rows {
		res.Events = append(res.Events, Event{ID: r.UID, Decision: r.Decision})
	}
	res.Truncated = more
	res.setNext(SourceStore)
	return nil
}

func (q Query) readGateway(ctx context.Context, cur *cursor, res *Result) {
	if q.Gateway == nil {
		res.Coverage.GatewayErr = errNoGateway
		return
	}
	ds, err := q.Gateway(ctx, q.Sandbox, q.Since)
	if err != nil {
		res.Coverage.GatewayErr = err
		return
	}
	events := gatewayEvents(ds)
	if len(events) > 0 && events[0].Time.After(q.Since) {
		res.Coverage.GatewayStart = events[0].Time
	}
	res.Events, res.Truncated = pageEvents(events, cur, q.Limit)
	res.setNext(SourceGateway)
}

// ReadStoreCoverage fills the fields of c that only the telemetry store can answer.
func ReadStoreCoverage(ctx context.Context, store *telemetry.Store, name string, since, now time.Time, retention time.Duration, c *Coverage) error {
	var err error
	if c.Drops, err = ReadDrops(ctx, store, name, since); err != nil {
		return err
	}
	earliest, ok, err := store.EarliestDecision(ctx, name)
	if err != nil {
		return err
	}
	if ok && earliest.After(since) {
		c.HistoryStart = earliest
	}
	c.Retention = retention
	c.Pruned = since.Before(now.Add(-retention))
	return nil
}

// ReadDrops reports events lost before or during receipt: exporter drops and decisions with no sandbox.
func ReadDrops(ctx context.Context, store *telemetry.Store, name string, since time.Time) (Drops, error) {
	ids, err := store.SandboxIDs(ctx, name)
	if err != nil {
		return Drops{}, err
	}
	gaps, err := store.Gaps(ctx, ids, since)
	if err != nil {
		return Drops{}, err
	}
	var d Drops
	for _, g := range gaps {
		drop := &d.Own
		switch {
		case g.SandboxID == "":
			drop = &d.GatewayWide
		case g.Unknown:
			drop = &d.Unknown
		}
		drop.Count += g.Dropped
		if drop.Since.IsZero() || g.Since.Before(drop.Since) {
			drop.Since = g.Since
		}
	}
	if d.Unattributed, err = store.Unattributed(ctx, since); err != nil {
		return Drops{}, err
	}
	return d, nil
}
