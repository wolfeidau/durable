package telemetry

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/wolfeidau/durable/internal/otlp"
	"github.com/wolfeidau/durable/internal/sandbox/openshell"
)

const schema = `
CREATE TABLE IF NOT EXISTS decisions (
	uid TEXT PRIMARY KEY,
	sandbox_name TEXT NOT NULL,
	sandbox_id TEXT NOT NULL,
	time_ms INTEGER NOT NULL,
	observed_ms INTEGER NOT NULL,
	allowed INTEGER NOT NULL,
	kind TEXT NOT NULL,
	activity TEXT NOT NULL,
	binary TEXT NOT NULL,
	method TEXT NOT NULL,
	host TEXT NOT NULL,
	path TEXT NOT NULL,
	pattern TEXT NOT NULL,
	policy TEXT NOT NULL,
	reason TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS decisions_sandbox_time ON decisions (sandbox_name, time_ms);
CREATE INDEX IF NOT EXISTS decisions_time ON decisions (time_ms);
CREATE INDEX IF NOT EXISTS decisions_sandbox_id ON decisions (sandbox_id);
CREATE TABLE IF NOT EXISTS raw_events (
	id INTEGER PRIMARY KEY,
	uid TEXT UNIQUE,
	sandbox_name TEXT NOT NULL,
	sandbox_id TEXT NOT NULL,
	time_ms INTEGER NOT NULL,
	received_ms INTEGER NOT NULL,
	record TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS raw_events_time ON raw_events (time_ms);
CREATE INDEX IF NOT EXISTS raw_events_sandbox_time ON raw_events (sandbox_name, time_ms);
CREATE TABLE IF NOT EXISTS daily (
	day TEXT NOT NULL,
	sandbox_name TEXT NOT NULL,
	allowed INTEGER NOT NULL,
	kind TEXT NOT NULL,
	activity TEXT NOT NULL,
	binary TEXT NOT NULL,
	method TEXT NOT NULL,
	host TEXT NOT NULL,
	pattern TEXT NOT NULL,
	policy TEXT NOT NULL,
	count INTEGER NOT NULL,
	PRIMARY KEY (day, sandbox_name, allowed, kind, activity, binary, method, host, pattern, policy)
);
CREATE TABLE IF NOT EXISTS gaps (
	id INTEGER PRIMARY KEY,
	sandbox_id TEXT NOT NULL,
	observed_ms INTEGER NOT NULL,
	since_ms INTEGER NOT NULL,
	dropped INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS gaps_observed ON gaps (observed_ms);
CREATE UNIQUE INDEX IF NOT EXISTS gaps_unique ON gaps (sandbox_id, since_ms, dropped);
CREATE TABLE IF NOT EXISTS unattributed (
	day TEXT PRIMARY KEY,
	count INTEGER NOT NULL
);
`

const topDenied = 3

// Event is one received record. It is a gap when Gap is set, a decision when Decision is set, and only archived otherwise.
type Event struct {
	Decision    *openshell.Decision
	UID         string
	SandboxName string // empty when unknown
	SandboxID   string
	Time        time.Time // the event's own time; zero falls back to Observed
	Observed    time.Time
	Received    time.Time // when the receiver got it; zero falls back to Observed
	Raw         []byte    // the full OTLP record, kept as received so it can be re-derived; the gateway keeps no copy
	Gap         *otlp.Gap // when set, only SandboxID and Observed are used for the derived row
}

// RawEvent is one archived OTLP record.
type RawEvent struct {
	UID    string // empty when the record had none
	Time   time.Time
	Record json.RawMessage
}

// Day is the decision counts for one UTC day.
type Day struct {
	Day       string // UTC YYYY-MM-DD
	Allowed   int
	Denied    int
	TopDenied []HostCount // up to 3 denied hosts by count, ties by host
}

type HostCount struct {
	Host  string
	Count int
}

// GapInfo records that the exporter dropped events for a sandbox, or gateway-wide when SandboxID is empty.
type GapInfo struct {
	SandboxID string
	Observed  time.Time
	Since     time.Time
	Dropped   int64
	// Unknown is set when SandboxID matches no stored decision, so the gap may belong to any sandbox.
	Unknown bool
}

// Store holds sandbox network decisions received over OTLP.
type Store struct {
	db *sql.DB
}

// Open opens or creates the store at path and migrates it.
func Open(ctx context.Context, path string) (*Store, error) {
	// Telemetry, not session state, so NORMAL is enough.
	db, err := open(fmt.Sprintf("file:%s?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000&_txlock=immediate", escape(path)))
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to migrate telemetry store: %w", err)
	}
	return &Store{db: db}, nil
}

// OpenReadOnly opens an existing store without creating or migrating it.
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	db, err := open(fmt.Sprintf("file:%s?mode=ro&_busy_timeout=5000", escape(path)))
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to open telemetry store: %w", err)
	}
	return &Store{db: db}, nil
}

// SQLite treats ?, # and % in a file: URI specially, so escape the path.
func escape(path string) string {
	return (&url.URL{Path: path}).EscapedPath()
}

func open(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open telemetry store: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (e Event) at() time.Time {
	if e.Time.IsZero() {
		return e.Observed
	}
	return e.Time
}

// Add stores evs in one transaction. Events before notBefore are skipped, because their rows may already have been
// pruned and a replay would count them again; tooOld is how many. unattributed is how many decisions it counted in the
// unattributed table instead of storing. A repeated UID is skipped, so it counts once.
func (s *Store) Add(ctx context.Context, evs []Event, notBefore time.Time) (unattributed, tooOld int, err error) {
	if len(evs) == 0 {
		return 0, 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to begin telemetry transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, ev := range evs {
		if ev.at().Before(notBefore) {
			tooOld++
			continue
		}
		fresh, err := addRaw(ctx, tx, ev)
		if err != nil {
			return 0, 0, err
		}
		if !fresh {
			continue
		}
		switch {
		case ev.Gap != nil:
			_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO gaps (sandbox_id, observed_ms, since_ms, dropped) VALUES (?, ?, ?, ?)`,
				ev.SandboxID, ev.Observed.UnixMilli(), ev.Gap.Since.UnixMilli(), ev.Gap.Dropped)
			if err != nil {
				return 0, 0, fmt.Errorf("failed to insert gap: %w", err)
			}
		case ev.Decision == nil:
		case ev.UID == "" || ev.SandboxName == "":
			unattributed++
			_, err = tx.ExecContext(ctx, `INSERT INTO unattributed (day, count) VALUES (?, 1)
				ON CONFLICT DO UPDATE SET count = count + 1`, day(ev.at()))
			if err != nil {
				return 0, 0, fmt.Errorf("failed to count unattributed decision: %w", err)
			}
		default:
			if err := addDecision(ctx, tx, ev); err != nil {
				return 0, 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("failed to commit telemetry transaction: %w", err)
	}
	return unattributed, tooOld, nil
}

// addRaw archives the record and reports false when its UID was already archived, which makes raw_events the dedup gate.
func addRaw(ctx context.Context, tx *sql.Tx, ev Event) (bool, error) {
	var uid any
	if ev.UID != "" {
		uid = ev.UID
	}
	received := ev.Received
	if received.IsZero() {
		received = ev.Observed
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO raw_events (uid, sandbox_name, sandbox_id, time_ms, received_ms, record)
		VALUES (?, ?, ?, ?, ?, ?)`, uid, ev.SandboxName, ev.SandboxID, ev.at().UnixMilli(), received.UnixMilli(), string(ev.Raw))
	if err != nil {
		return false, fmt.Errorf("failed to insert raw event: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to count inserted raw events: %w", err)
	}
	return n == 1, nil
}

func addDecision(ctx context.Context, tx *sql.Tx, ev Event) error {
	d := *ev.Decision
	if d.Time.IsZero() {
		d.Time = ev.Observed
	}
	pattern := d.Pattern()
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO decisions
		(uid, sandbox_name, sandbox_id, time_ms, observed_ms, allowed, kind, activity, binary, method, host, path, pattern, policy, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.UID, ev.SandboxName, ev.SandboxID, d.Time.UnixMilli(), ev.Observed.UnixMilli(), d.Allowed, d.Kind, d.Activity, d.Binary,
		d.Method, d.Host, d.Path, pattern, d.Policy, d.Reason)
	if err != nil {
		return fmt.Errorf("failed to insert decision: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO daily (day, sandbox_name, allowed, kind, activity, binary, method, host, pattern, policy, count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
		ON CONFLICT DO UPDATE SET count = count + 1`,
		day(d.Time), ev.SandboxName, d.Allowed, d.Kind, d.Activity, d.Binary, d.Method, d.Host, pattern, d.Policy)
	if err != nil {
		return fmt.Errorf("failed to update daily count: %w", err)
	}
	return nil
}

func day(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// Prune deletes decisions, raw events and gaps older than before, keeping the daily counts.
func (s *Store) Prune(ctx context.Context, before time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin telemetry transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var total int64
	for _, q := range []string{`DELETE FROM decisions WHERE time_ms < ?`, `DELETE FROM raw_events WHERE time_ms < ?`, `DELETE FROM gaps WHERE observed_ms < ?`} {
		res, err := tx.ExecContext(ctx, q, before.UnixMilli())
		if err != nil {
			return 0, fmt.Errorf("failed to prune telemetry: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("failed to count pruned rows: %w", err)
		}
		total += n
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit telemetry transaction: %w", err)
	}
	return total, nil
}

// Decisions returns the sandbox's decisions at or after since, oldest first.
func (s *Store) Decisions(ctx context.Context, sandboxName string, since time.Time) ([]openshell.Decision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT time_ms, allowed, kind, activity, binary, method, host, path, policy, reason
		FROM decisions WHERE sandbox_name = ? AND time_ms >= ? ORDER BY time_ms, uid`, sandboxName, since.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("failed to query decisions: %w", err)
	}
	defer rows.Close()
	var out []openshell.Decision
	for rows.Next() {
		var d openshell.Decision
		var ms int64
		if err := rows.Scan(&ms, &d.Allowed, &d.Kind, &d.Activity, &d.Binary, &d.Method, &d.Host, &d.Path, &d.Policy, &d.Reason); err != nil {
			return nil, fmt.Errorf("failed to scan decision: %w", err)
		}
		d.Time = time.UnixMilli(ms).UTC()
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read decisions: %w", err)
	}
	return out, nil
}

// RawEvents returns the archived records at or after since for the sandbox, oldest first. It includes records with no
// sandbox name whose sandbox ID is in sandboxIDs, such as the sandbox's gap records.
func (s *Store) RawEvents(ctx context.Context, sandboxName string, sandboxIDs []string, since time.Time) ([]RawEvent, error) {
	args := []any{since.UnixMilli(), sandboxName}
	where := `sandbox_name = ?`
	var marks []string
	for _, id := range sandboxIDs {
		if id != "" {
			marks = append(marks, "?")
			args = append(args, id)
		}
	}
	if len(marks) > 0 {
		where += ` OR (sandbox_name = '' AND sandbox_id IN (` + strings.Join(marks, ", ") + `))`
	}
	rows, err := s.db.QueryContext(ctx, `SELECT uid, time_ms, record FROM raw_events
		WHERE time_ms >= ? AND (`+where+`) ORDER BY time_ms, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query raw events: %w", err)
	}
	defer rows.Close()
	var out []RawEvent
	for rows.Next() {
		var e RawEvent
		var uid sql.NullString
		var ms int64
		var record string
		if err := rows.Scan(&uid, &ms, &record); err != nil {
			return nil, fmt.Errorf("failed to scan raw event: %w", err)
		}
		e.UID, e.Time, e.Record = uid.String, time.UnixMilli(ms).UTC(), json.RawMessage(record)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read raw events: %w", err)
	}
	return out, nil
}

// Daily returns per-day counts from the UTC day of from onward, oldest first.
func (s *Store) Daily(ctx context.Context, sandboxName string, from time.Time) ([]Day, error) {
	days, idx, err := s.dailyTotals(ctx, sandboxName, from)
	if err != nil {
		return nil, err
	}
	if err := s.deniedHosts(ctx, sandboxName, from, days, idx); err != nil {
		return nil, err
	}
	for i := range days {
		slices.SortFunc(days[i].TopDenied, func(a, b HostCount) int {
			if a.Count != b.Count {
				return b.Count - a.Count
			}
			return strings.Compare(a.Host, b.Host)
		})
		if len(days[i].TopDenied) > topDenied {
			days[i].TopDenied = days[i].TopDenied[:topDenied]
		}
	}
	return days, nil
}

func (s *Store) dailyTotals(ctx context.Context, sandboxName string, from time.Time) ([]Day, map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT day, allowed, SUM(count) FROM daily
		WHERE sandbox_name = ? AND day >= ? GROUP BY day, allowed ORDER BY day`, sandboxName, day(from))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to query daily counts: %w", err)
	}
	defer rows.Close()
	var days []Day
	idx := map[string]int{}
	for rows.Next() {
		var dy string
		var allowed bool
		var n int
		if err := rows.Scan(&dy, &allowed, &n); err != nil {
			return nil, nil, fmt.Errorf("failed to scan daily count: %w", err)
		}
		i, ok := idx[dy]
		if !ok {
			i = len(days)
			idx[dy] = i
			days = append(days, Day{Day: dy})
		}
		if allowed {
			days[i].Allowed = n
		} else {
			days[i].Denied = n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("failed to read daily counts: %w", err)
	}
	return days, idx, nil
}

func (s *Store) deniedHosts(ctx context.Context, sandboxName string, from time.Time, days []Day, idx map[string]int) error {
	rows, err := s.db.QueryContext(ctx, `SELECT day, host, SUM(count) FROM daily
		WHERE sandbox_name = ? AND day >= ? AND allowed = 0 GROUP BY day, host`, sandboxName, day(from))
	if err != nil {
		return fmt.Errorf("failed to query denied hosts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var dy string
		var hc HostCount
		if err := rows.Scan(&dy, &hc.Host, &hc.Count); err != nil {
			return fmt.Errorf("failed to scan denied host: %w", err)
		}
		// A DNS refusal has no port and the connect that follows it does, so both count against the hostname.
		if h, _, err := net.SplitHostPort(hc.Host); err == nil {
			hc.Host = h
		}
		top := &days[idx[dy]].TopDenied
		if i := slices.IndexFunc(*top, func(e HostCount) bool { return e.Host == hc.Host }); i >= 0 {
			(*top)[i].Count += hc.Count
		} else {
			*top = append(*top, hc)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read denied hosts: %w", err)
	}
	return nil
}

// SandboxIDs returns the distinct sandbox IDs seen under sandboxName, since a recreated sandbox keeps its name.
func (s *Store) SandboxIDs(ctx context.Context, sandboxName string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT sandbox_id FROM decisions WHERE sandbox_name = ? ORDER BY sandbox_id`, sandboxName)
	if err != nil {
		return nil, fmt.Errorf("failed to query sandbox ids: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan sandbox id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read sandbox ids: %w", err)
	}
	return ids, nil
}

// Gaps returns gaps observed at or after since for sandboxIDs, gateway-wide gaps, and gaps for sandbox IDs that no stored
// decision mentions (flagged Unknown), oldest first.
func (s *Store) Gaps(ctx context.Context, sandboxIDs []string, since time.Time) ([]GapInfo, error) {
	args := []any{since.UnixMilli()}
	var marks strings.Builder
	marks.WriteString("''")
	for _, id := range sandboxIDs {
		marks.WriteString(", ?")
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sandbox_id, observed_ms, since_ms, dropped, unknown FROM (
			SELECT g.*, g.sandbox_id != '' AND NOT EXISTS (SELECT 1 FROM decisions d WHERE d.sandbox_id = g.sandbox_id) AS unknown FROM gaps g)
		WHERE observed_ms >= ? AND (sandbox_id IN (`+marks.String()+`) OR unknown) ORDER BY observed_ms, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query gaps: %w", err)
	}
	defer rows.Close()
	var out []GapInfo
	for rows.Next() {
		var g GapInfo
		var obs, since int64
		if err := rows.Scan(&g.SandboxID, &obs, &since, &g.Dropped, &g.Unknown); err != nil {
			return nil, fmt.Errorf("failed to scan gap: %w", err)
		}
		g.Observed, g.Since = time.UnixMilli(obs).UTC(), time.UnixMilli(since).UTC()
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read gaps: %w", err)
	}
	return out, nil
}

// Unattributed returns how many decisions could not be matched to a sandbox since the UTC day of since.
func (s *Store) Unattributed(ctx context.Context, since time.Time) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(count), 0) FROM unattributed WHERE day >= ?`, day(since)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("failed to query unattributed decisions: %w", err)
	}
	return n, nil
}

// EarliestDecision returns the time of the sandbox's oldest stored decision; ok is false when it has none.
func (s *Store) EarliestDecision(ctx context.Context, sandboxName string) (t time.Time, ok bool, err error) {
	var ms sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(time_ms) FROM decisions WHERE sandbox_name = ?`, sandboxName).Scan(&ms); err != nil {
		return time.Time{}, false, fmt.Errorf("failed to query earliest decision: %w", err)
	}
	if !ms.Valid {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(ms.Int64).UTC(), true, nil
}

// FirstDay returns the UTC day of the sandbox's first daily row, which outlives pruning; ok is false when it has none.
func (s *Store) FirstDay(ctx context.Context, sandboxName string) (dy string, ok bool, err error) {
	var v sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(day) FROM daily WHERE sandbox_name = ?`, sandboxName).Scan(&v); err != nil {
		return "", false, fmt.Errorf("failed to query first day: %w", err)
	}
	return v.String, v.Valid, nil
}
