package durable

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/wolfeidau/durable/internal/rawjson"
)

// EventKind says which state change an Event records.
type EventKind string

const (
	EventSubmission  EventKind = "submission"   // submitted or changed status
	EventMessage     EventKind = "message"      // transcript entry appended
	EventCallStarted EventKind = "call_started" // tool call committed to run
	// EventCallReplayed is a ReplaySafe call running again after a crash, so each execution has an event.
	EventCallReplayed EventKind = "call_replayed"
	// EventCallDone is a call's result, whether it ran or not; the message event before it has the text.
	EventCallDone EventKind = "call_done"
	// EventCompacted records a compaction; the summary is in the session database, not the event.
	EventCompacted EventKind = "compacted"
	// EventCompactionFailed records a compactor error that did not stop the run; Error has the text.
	EventCompactionFailed EventKind = "compaction_failed"
)

// Event is one committed state change. Events are append-only and written in
// the same transaction as the change they describe.
type Event struct {
	Seq        int64     `json:"-"` // strictly increasing per session; use it to resume
	Kind       EventKind `json:"kind"`
	Submission int64     `json:"submission"` // submission seq, set on every event

	Status SubmissionStatus `json:"status,omitempty"` // submission events
	Error  string           `json:"error,omitempty"`  // submission events when failed, and compaction_failed

	Message *Message `json:"message,omitempty"` // message events; Raw is omitted, History has it
	// Call is the input that actually runs, so it can contain secrets a gate added.
	Call      *ToolCall `json:"call,omitempty"`
	Rewritten bool      `json:"rewritten,omitempty"` // a gate changed the input
	IsError   bool      `json:"is_error,omitempty"`  // call_done: the result is an error
}

// Events returns up to limit events after seq, oldest first. A limit of zero
// or less returns all of them.
func (h *Harness) Events(ctx context.Context, after int64, limit int) ([]Event, error) {
	return loadEvents(ctx, h.db, after, limit)
}

func addEvent(ctx context.Context, tx *sql.Tx, e Event) error {
	// HTML escaping would rewrite tool input, which must read back unchanged.
	body, err := rawjson.Marshal(e)
	if err != nil {
		return fmt.Errorf("failed to encode event: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (body) VALUES (?)`, string(body)); err != nil {
		return fmt.Errorf("failed to insert event: %w", err)
	}
	return nil
}

// loadEvents closes its cursor before returning so an Observer can query on the single connection.
func loadEvents(ctx context.Context, q querier, after int64, limit int) ([]Event, error) {
	query := `SELECT seq, body FROM events WHERE seq > ? ORDER BY seq`
	args := []any{after}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var body string
		if err := rows.Scan(&e.Seq, &body); err != nil {
			return nil, fmt.Errorf("failed to read event: %w", err)
		}
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			return nil, fmt.Errorf("durable: corrupt event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read events: %w", err)
	}
	return out, nil
}

// deliver runs after a commit, so a failure here must never fail the commit: observedSeq stays
// put and the next commit, if there is one, retries. Events always has what was missed.
func (h *Harness) deliver(ctx context.Context) {
	if h.opts.Observer == nil {
		return
	}
	h.observeMu.Lock()
	defer h.observeMu.Unlock()
	events, err := loadEvents(context.WithoutCancel(ctx), h.db, h.observedSeq, 0)
	if err != nil {
		return
	}
	for _, e := range events {
		h.opts.Observer(e)
		h.observedSeq = e.Seq
	}
}
