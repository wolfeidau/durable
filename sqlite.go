package durable

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3" // swap for modernc.org/sqlite if you want no cgo
)

// Open opens (or creates) a session database at path with settings chosen
// for durability: WAL journal, synchronous=FULL so a commit survives power
// loss, and IMMEDIATE transactions so the lease fence takes the write lock
// before anything else in the transaction runs.
func Open(ctx context.Context, path string, opts Options) (*Harness, error) {
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000&_txlock=immediate&_foreign_keys=on", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: the harness is single-writer by design, and it keeps
	// reads inside the process consistent with its own writes.
	db.SetMaxOpenConns(1)
	h, err := OpenDB(ctx, db, opts)
	if err != nil {
		db.Close()
		return nil, err
	}
	return h, nil
}
