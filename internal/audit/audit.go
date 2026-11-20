// Package audit reads a sandbox's network decisions from the gateway log until an external retention source is wired in.
package audit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wolfeidau/durable/internal/sandbox/openshell"
)

// Gateway reads the sandbox's decisions from the gateway log, which keeps only recent lines.
type Gateway func(ctx context.Context, name string, since time.Time) ([]openshell.Decision, error)

// Coverage says what the decisions do not show.
type Coverage struct {
	// GatewayErr is why the gateway log could not be read.
	GatewayErr error
	// GatewayStart is the first gateway decision when it is later than the window start, so earlier ones are missing.
	GatewayStart time.Time
}

// Query selects the decisions to read.
type Query struct {
	Gateway Gateway // nil when no gateway reader is configured
	Sandbox string
	Since   time.Time
	// After must be empty: the gateway log has no cursor.
	After string
	// Limit caps the events returned; zero or less returns them all.
	Limit int
}

// Event is one decision with the id that orders it: a hash of the decision's content, because the log has no uid.
type Event struct {
	ID string
	openshell.Decision
}

// Result is the events and their coverage.
type Result struct {
	// Events are oldest first by (time, id).
	Events   []Event
	Coverage Coverage
	// Truncated means Limit left older events in the window out.
	Truncated bool
}

var errNoGateway = errors.New("no gateway log reader is configured")

// ErrBadCursor marks a cursor that is malformed or that this server cannot resume.
var ErrBadCursor = errors.New("invalid cursor")

// Read returns the gateway log's decisions. A gateway failure is reported in Coverage.GatewayErr, not returned.
func Read(ctx context.Context, q Query) (Result, error) {
	if q.After != "" {
		return Result{}, fmt.Errorf("%w: cursors are not supported by the gateway log", ErrBadCursor)
	}
	var res Result
	q.readGateway(ctx, &res)
	return res, nil
}

func (q Query) readGateway(ctx context.Context, res *Result) {
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
	if q.Limit > 0 && len(events) > q.Limit {
		events, res.Truncated = events[len(events)-q.Limit:], true
	}
	res.Events = events
}
