// Package audit reads a sandbox's network decisions and says what the result cannot show.
package audit

import (
	"context"
	"errors"
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
}

// Result is the decisions, oldest first, and their coverage.
type Result struct {
	Decisions []openshell.Decision
	Coverage  Coverage
}

var errNoGateway = errors.New("no gateway log reader is configured")

// Read prefers the telemetry store and falls back to the gateway log when the store has none.
// A gateway failure is reported in Coverage.GatewayErr, not returned, because the store may still add coverage.
func Read(ctx context.Context, q Query) (Result, error) {
	res := Result{Coverage: Coverage{Source: SourceStore, StoreAvailable: q.Store != nil}}
	if q.Store != nil {
		ds, err := q.Store.Decisions(ctx, q.Sandbox, q.Since)
		if err != nil {
			return Result{}, err
		}
		res.Decisions = ds
	}
	if len(res.Decisions) == 0 {
		res.Coverage.Source = SourceGateway
		res.Coverage.StoreEmpty = q.Store != nil
		res.Decisions, res.Coverage.GatewayErr = q.gateway(ctx)
		if len(res.Decisions) > 0 && res.Decisions[0].Time.After(q.Since) {
			res.Coverage.GatewayStart = res.Decisions[0].Time
		}
	}
	if q.Store == nil {
		return res, nil
	}
	if err := ReadStoreCoverage(ctx, q.Store, q.Sandbox, q.Since, q.Now, q.Retention, &res.Coverage); err != nil {
		return Result{}, err
	}
	return res, nil
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

func (q Query) gateway(ctx context.Context) ([]openshell.Decision, error) {
	if q.Gateway == nil {
		return nil, errNoGateway
	}
	return q.Gateway(ctx, q.Sandbox, q.Since)
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
