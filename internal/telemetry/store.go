package telemetry

import (
	"context"
	"database/sql"
	"fmt"
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
	reason TEXT NOT NULL,
	body TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS decisions_sandbox_time ON decisions (sandbox_name, time_ms);
CREATE INDEX IF NOT EXISTS decisions_time ON decisions (time_ms);
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
`

const topDenied = 3

// Event is one received record: a decision, or a gap when Gap is set.
type Event struct {
	Decision    openshell.Decision
	UID         string
	SandboxName string
	SandboxID   string
	Observed    time.Time
	Body        []byte
	Gap         *otlp.Gap // when set, only SandboxID and Observed are used
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

// Add stores evs in one transaction; decisions without a UID or sandbox name are skipped, and duplicate UIDs count once.
func (s *Store) Add(ctx context.Context, evs []Event) error {
	if len(evs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin telemetry transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, ev := range evs {
		if ev.Gap != nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO gaps (sandbox_id, observed_ms, since_ms, dropped) VALUES (?, ?, ?, ?)`,
				ev.SandboxID, ev.Observed.UnixMilli(), ev.Gap.Since.UnixMilli(), ev.Gap.Dropped)
			if err != nil {
				return fmt.Errorf("failed to insert gap: %w", err)
			}
			continue
		}
		if ev.UID == "" || ev.SandboxName == "" {
			continue
		}
		if err := addDecision(ctx, tx, ev); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit telemetry transaction: %w", err)
	}
	return nil
}

func addDecision(ctx context.Context, tx *sql.Tx, ev Event) error {
	d := ev.Decision
	if d.Time.IsZero() {
		d.Time = ev.Observed
	}
	pattern := d.Pattern()
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO decisions
		(uid, sandbox_name, sandbox_id, time_ms, observed_ms, allowed, kind, activity, binary, method, host, path, pattern, policy, reason, body)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.UID, ev.SandboxName, ev.SandboxID, d.Time.UnixMilli(), ev.Observed.UnixMilli(), d.Allowed, d.Kind, d.Activity, d.Binary,
		d.Method, d.Host, d.Path, pattern, d.Policy, d.Reason, string(ev.Body))
	if err != nil {
		return fmt.Errorf("failed to insert decision: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("failed to count inserted decisions: %w", err)
	} else if n != 1 {
		return nil
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

// Prune deletes decisions and gaps older than before, keeping the daily counts.
func (s *Store) Prune(ctx context.Context, before time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin telemetry transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var total int64
	for _, q := range []string{`DELETE FROM decisions WHERE time_ms < ?`, `DELETE FROM gaps WHERE observed_ms < ?`} {
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
		days[idx[dy]].TopDenied = append(days[idx[dy]].TopDenied, hc)
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

// Gaps returns gaps observed at or after since for sandboxIDs and gateway-wide gaps, oldest first.
func (s *Store) Gaps(ctx context.Context, sandboxIDs []string, since time.Time) ([]GapInfo, error) {
	args := []any{since.UnixMilli()}
	var marks strings.Builder
	marks.WriteString("''")
	for _, id := range sandboxIDs {
		marks.WriteString(", ?")
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sandbox_id, observed_ms, since_ms, dropped FROM gaps
		WHERE observed_ms >= ? AND sandbox_id IN (`+marks.String()+`) ORDER BY observed_ms, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query gaps: %w", err)
	}
	defer rows.Close()
	var out []GapInfo
	for rows.Next() {
		var g GapInfo
		var obs, since int64
		if err := rows.Scan(&g.SandboxID, &obs, &since, &g.Dropped); err != nil {
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
