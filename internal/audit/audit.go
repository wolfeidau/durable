// Package audit reads a sandbox's network decisions from the gateway log until an external retention source is wired in.
package audit

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
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
	if q.Gateway == nil {
		return Result{Coverage: Coverage{GatewayErr: errNoGateway}}, nil
	}
	ds, err := q.Gateway(ctx, q.Sandbox, q.Since)
	if err != nil {
		return Result{Coverage: Coverage{GatewayErr: err}}, nil //nolint:nilerr // gateway failure is reported in Coverage.GatewayErr, not returned
	}
	var res Result
	events := gatewayEvents(ds)
	if len(events) > 0 && events[0].Time.After(q.Since) {
		res.Coverage.GatewayStart = events[0].Time
	}
	if q.Limit > 0 && len(events) > q.Limit {
		events, res.Truncated = events[len(events)-q.Limit:], true
	}
	res.Events = events
	return res, nil
}

// gatewayEvents gives each log decision an id derived from its content, ordered by (time, id).
// Identical decisions in the same millisecond get a counter, in log order. The ids let a client dedupe repeated reads;
// they are not stable once the log drops lines, so there is no cursor.
func gatewayEvents(ds []openshell.Decision) []Event {
	seen := map[string]int{}
	out := make([]Event, len(ds))
	for i, d := range ds {
		sum := sha256.Sum256(fmt.Appendf(nil, "%d\x00%t\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s",
			d.Time.UnixMilli(), d.Allowed, d.Kind, d.Activity, d.Binary, d.Method, d.Host, d.Path, d.Policy, d.Reason))
		key := hex.EncodeToString(sum[:16])
		n := seen[key]
		seen[key]++
		out[i] = Event{ID: "g-" + key + "-" + strconv.Itoa(n), Decision: d}
	}
	slices.SortStableFunc(out, func(a, b Event) int {
		if c := cmp.Compare(a.Time.UnixMilli(), b.Time.UnixMilli()); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}
