package durable

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrLeaseLost means another process took over this session. Whatever this
// process was doing must stop; the new owner will continue the run.
var ErrLeaseLost = errors.New("durable: lease lost to another process")

// One database file is one session, the same shape as Pi Durable's local
// agent and a Durable Object: no session_id column anywhere.
const schema = `
CREATE TABLE IF NOT EXISTS lease (
	id         INTEGER PRIMARY KEY CHECK (id = 1),
	owner      TEXT    NOT NULL,
	expires_at INTEGER NOT NULL           -- unix millis
);
CREATE TABLE IF NOT EXISTS submissions (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	request_id TEXT    NOT NULL UNIQUE,   -- exactly-once input
	text       TEXT    NOT NULL,
	status     TEXT    NOT NULL,          -- queued | running | done | failed
	result     TEXT,
	error      TEXT,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
	seq            INTEGER PRIMARY KEY AUTOINCREMENT,
	submission_seq INTEGER NOT NULL REFERENCES submissions(seq),
	role           TEXT    NOT NULL,
	body           TEXT    NOT NULL       -- JSON Message
);
CREATE TABLE IF NOT EXISTS tool_calls (
	message_seq INTEGER NOT NULL REFERENCES messages(seq),
	position    INTEGER NOT NULL,         -- order within the assistant message
	call_id     TEXT    NOT NULL,         -- provider ID; unique only per message
	name        TEXT    NOT NULL,
	input       TEXT    NOT NULL,
	status      TEXT    NOT NULL,         -- pending | started | done
	result      TEXT,
	is_error    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (message_seq, position)
);
`

const (
	callPending = "pending" // recorded with the assistant message, not run yet
	callStarted = "started" // intent committed; the effect may or may not have happened
	callDone    = "done"    // result committed
)

func migrate(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, schema)
	return err
}

func nowMillis() int64 { return time.Now().UnixMilli() }

// acquireLease takes the session if nobody holds it, the lease expired, or we
// already own it. It returns false if someone else holds a live lease.
func acquireLease(ctx context.Context, db *sql.DB, owner string, ttl time.Duration) (bool, error) {
	now := nowMillis()
	res, err := db.ExecContext(ctx, `
		INSERT INTO lease (id, owner, expires_at) VALUES (1, ?, ?)
		ON CONFLICT (id) DO UPDATE SET owner = excluded.owner, expires_at = excluded.expires_at
		WHERE lease.expires_at < ? OR lease.owner = excluded.owner`,
		owner, now+ttl.Milliseconds(), now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func renewLease(ctx context.Context, db *sql.DB, owner string, ttl time.Duration) error {
	res, err := db.ExecContext(ctx,
		`UPDATE lease SET expires_at = ? WHERE id = 1 AND owner = ?`,
		nowMillis()+ttl.Milliseconds(), owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrLeaseLost
	}
	return nil
}

func releaseLease(ctx context.Context, db *sql.DB, owner string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM lease WHERE id = 1 AND owner = ?`, owner)
	return err
}

// commit runs fn in a write transaction fenced by the lease: the first
// statement re-checks ownership, so a process that lost its lease (paused,
// partitioned, presumed dead) can never write over the new owner's work.
// Everything fn writes lands atomically or not at all.
func (h *Harness) commit(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	res, err := tx.ExecContext(ctx,
		`UPDATE lease SET expires_at = ? WHERE id = 1 AND owner = ?`,
		nowMillis()+h.opts.LeaseTTL.Milliseconds(), h.owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrLeaseLost
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// --- reads -------------------------------------------------------------

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func scanSubmission(row interface{ Scan(...any) error }) (Submission, error) {
	var s Submission
	var status string
	var result, errText sql.NullString
	if err := row.Scan(&s.Seq, &s.RequestID, &s.Text, &status, &result, &errText); err != nil {
		return s, err
	}
	s.Status = SubmissionStatus(status)
	s.Result = result.String
	s.Error = errText.String
	return s, nil
}

const submissionCols = `seq, request_id, text, status, result, error`

func getSubmission(ctx context.Context, q querier, requestID string) (Submission, error) {
	return scanSubmission(q.QueryRowContext(ctx,
		`SELECT `+submissionCols+` FROM submissions WHERE request_id = ?`, requestID))
}

// nextUnfinished returns the oldest submission that still needs work.
// Submissions run strictly in order, like a queue.
func nextUnfinished(ctx context.Context, q querier) (Submission, bool, error) {
	s, err := scanSubmission(q.QueryRowContext(ctx,
		`SELECT `+submissionCols+` FROM submissions
		 WHERE status IN ('queued', 'running') ORDER BY seq LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return s, false, nil
	}
	return s, err == nil, err
}

func loadHistory(ctx context.Context, q querier) ([]Message, error) {
	rows, err := q.QueryContext(ctx, `SELECT body FROM messages ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var m Message
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			return nil, fmt.Errorf("durable: corrupt message: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

type storedCall struct {
	ToolCall
	MessageSeq int64
	Position   int
	Status     string
}

// lastAssistantCalls returns the tool calls of the newest assistant message in
// a submission, or ok=false if the submission has no assistant message yet.
func lastAssistantCalls(ctx context.Context, q querier, submissionSeq int64) (calls []storedCall, ok bool, err error) {
	var msgSeq int64
	err = q.QueryRowContext(ctx,
		`SELECT seq FROM messages WHERE submission_seq = ? AND role = 'assistant'
		 ORDER BY seq DESC LIMIT 1`, submissionSeq).Scan(&msgSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rows, err := q.QueryContext(ctx,
		`SELECT call_id, position, name, input, status FROM tool_calls
		 WHERE message_seq = ? ORDER BY position`, msgSeq)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		c := storedCall{MessageSeq: msgSeq}
		var input string
		if err := rows.Scan(&c.ID, &c.Position, &c.Name, &input, &c.Status); err != nil {
			return nil, false, err
		}
		c.Input = json.RawMessage(input)
		calls = append(calls, c)
	}
	return calls, true, rows.Err()
}

// --- writes (always inside commit) ------------------------------------

func appendMessage(ctx context.Context, tx *sql.Tx, submissionSeq int64, m Message) (int64, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO messages (submission_seq, role, body) VALUES (?, ?, ?)`,
		submissionSeq, string(m.Role), string(body))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func setSubmission(ctx context.Context, tx *sql.Tx, seq int64, status SubmissionStatus, result, errText string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE submissions SET status = ?, result = NULLIF(?, ''), error = NULLIF(?, '') WHERE seq = ?`,
		string(status), result, errText, seq)
	return err
}
