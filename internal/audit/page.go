package audit

import (
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/wolfeidau/durable/internal/sandbox/openshell"
	"github.com/wolfeidau/durable/internal/telemetry"
)

// cursor is a position in one source's (time, id) order. The source is part of it, because ids from the two do not mix.
type cursor struct {
	source Source
	pos    telemetry.Position
}

func (c cursor) String() string {
	tag := "s"
	if c.source == SourceGateway {
		tag = "g"
	}
	return base64.RawURLEncoding.EncodeToString([]byte(tag + ":" + strconv.FormatInt(c.pos.TimeMS, 10) + ":" + c.pos.UID))
}

// parseCursor returns nil for an empty cursor.
func parseCursor(s string) (*cursor, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: not a cursor from this API", ErrBadCursor)
	}
	// The id may contain colons, so only the first two separate fields.
	parts := strings.SplitN(string(raw), ":", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: not a cursor from this API", ErrBadCursor)
	}
	ms, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: not a cursor from this API", ErrBadCursor)
	}
	c := &cursor{pos: telemetry.Position{TimeMS: ms, UID: parts[2]}}
	switch parts[0] {
	case "s":
		c.source = SourceStore
	case "g":
		c.source = SourceGateway
	default:
		return nil, fmt.Errorf("%w: not a cursor from this API", ErrBadCursor)
	}
	return c, nil
}

// setNext points the cursor after the last event, and leaves an earlier cursor in place when there are none.
func (r *Result) setNext(src Source) {
	if len(r.Events) == 0 {
		return
	}
	last := r.Events[len(r.Events)-1]
	r.NextCursor = cursor{source: src, pos: telemetry.Position{TimeMS: last.Time.UnixMilli(), UID: last.ID}}.String()
}

// gatewayEvents gives each log decision an id derived from its content, ordered like the store by (time, id).
// Identical decisions in the same millisecond get a counter, in log order. The id is stable only while the log keeps
// the same lines, which is why gateway cursors cannot resume reliably.
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

// pageEvents applies the same cursor and limit semantics as the store to events already sorted by (time, id).
func pageEvents(events []Event, after *cursor, limit int) (page []Event, truncated bool) {
	if after != nil {
		events = slices.DeleteFunc(slices.Clone(events), func(e Event) bool {
			ms := e.Time.UnixMilli()
			return ms < after.pos.TimeMS || (ms == after.pos.TimeMS && e.ID <= after.pos.UID)
		})
	}
	if limit <= 0 || len(events) <= limit {
		return events, false
	}
	if after == nil {
		return events[len(events)-limit:], true
	}
	return events[:limit], true
}
