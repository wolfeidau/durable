package durable

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Status summarizes work in a session database.
type Status struct {
	PendingApprovals     int
	Unfinished           int              // queued plus running submissions
	LastSubmissionStatus SubmissionStatus // newest submission status, or empty if none
}

// ReadStatus reads work status without taking the lease,
// so it can read beside the process that owns the session. A list can be a moment stale.
func ReadStatus(ctx context.Context, path string) (Status, error) {
	db, err := openReadOnly(path)
	if err != nil {
		return Status{}, fmt.Errorf("failed to open session database: %w", err)
	}
	defer func() { _ = db.Close() }()

	var s Status
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals WHERE status = 'pending'`).Scan(&s.PendingApprovals); err != nil {
		return Status{}, fmt.Errorf("failed to count pending approvals: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM submissions WHERE status IN ('queued','running')`).Scan(&s.Unfinished); err != nil {
		return Status{}, fmt.Errorf("failed to count unfinished submissions: %w", err)
	}
	var last string
	if err := db.QueryRowContext(ctx, `SELECT status FROM submissions ORDER BY seq DESC LIMIT 1`).Scan(&last); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Status{}, fmt.Errorf("failed to read last submission status: %w", err)
	}
	s.LastSubmissionStatus = SubmissionStatus(last)
	return s, nil
}
