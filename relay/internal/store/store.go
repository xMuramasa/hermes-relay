// Package store keeps the relay's state in SQLite: devices, one-time pairing codes, runs and the per-session event
// log. The serve process and the pair/devices subcommands open the same file (WAL mode, busy timeout).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps the database.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// migrations[i] moves the schema from version i to i+1 (PRAGMA user_version).
var migrations = []string{
	`CREATE TABLE devices (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		token_hash BLOB NOT NULL UNIQUE,
		created_at INTEGER NOT NULL,
		last_seen_at INTEGER
	);
	CREATE TABLE pair_codes (
		code_hash BLOB PRIMARY KEY,
		name TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		used_at INTEGER
	);
	CREATE TABLE sessions (
		session_id TEXT PRIMARY KEY,
		last_seq INTEGER NOT NULL DEFAULT 0,
		pruned_seq INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE runs (
		run_id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		client_msg_id TEXT UNIQUE,
		device_id INTEGER,
		status TEXT NOT NULL,
		hermes_seq INTEGER NOT NULL DEFAULT -1,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE INDEX runs_status ON runs(status);
	CREATE TABLE events (
		session_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		run_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		data TEXT NOT NULL,
		ts INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		PRIMARY KEY (session_id, seq)
	) WITHOUT ROWID;
	CREATE INDEX events_created ON events(created_at);`,
	// Roles: devices paired before this keep working as viewers.
	`ALTER TABLE devices ADD COLUMN role TEXT NOT NULL DEFAULT 'viewer' CHECK (role IN ('manager', 'viewer'));
	ALTER TABLE pair_codes ADD COLUMN role TEXT NOT NULL DEFAULT 'viewer' CHECK (role IN ('manager', 'viewer'));`,
	// Tags: the relay's own, Hermes has none. rowid keeps the order they were set in.
	`CREATE TABLE session_tags (
		session_id TEXT NOT NULL,
		tag TEXT NOT NULL COLLATE NOCASE,
		UNIQUE (session_id, tag)
	);`,
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*Store, error) {
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// SetClock replaces the clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the database is usable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) ms() int64 { return s.now().UnixMilli() }

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer tx.Rollback()
	var v int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if v > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this relay (%d)", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	return tx.Commit()
}

// Version returns the schema version.
func (s *Store) Version() (int, error) {
	var v int
	err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	return v, err
}

// Errors.
var (
	ErrNotFound       = errors.New("not found")
	ErrNameExists     = errors.New("a device with that name exists")
	ErrTooManyDevices = errors.New("device limit reached")
	ErrInvalidCode    = errors.New("invalid, used or expired pairing code")
	ErrInvalidRole    = errors.New("role must be manager or viewer")
)

// Retention is how long events, finished runs and used codes are kept.
const Retention = 24 * time.Hour

// Cleanup deletes events and finished runs older than Retention and expired or used pairing codes.
func (s *Store) Cleanup(ctx context.Context) (events int64, err error) {
	cut := s.ms() - Retention.Milliseconds()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE sessions SET pruned_seq = MAX(pruned_seq,
		(SELECT MAX(seq) FROM events e WHERE e.session_id = sessions.session_id AND e.created_at < ?))
		WHERE session_id IN (SELECT DISTINCT session_id FROM events WHERE created_at < ?)`, cut, cut); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM events WHERE created_at < ?`, cut)
	if err != nil {
		return 0, err
	}
	events, _ = res.RowsAffected()
	if _, err = tx.ExecContext(ctx, `DELETE FROM runs WHERE status IN ('completed','failed','cancelled') AND updated_at < ?`, cut); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM pair_codes WHERE expires_at < ? OR used_at IS NOT NULL`, s.ms()); err != nil {
		return 0, err
	}
	return events, tx.Commit()
}
