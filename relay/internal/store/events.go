package store

import (
	"context"
	"database/sql"
	"errors"
)

// Event is one stored, numbered event.
type Event struct {
	SessionID string
	Seq       int64
	RunID     string
	Kind      string
	Data      []byte
	TS        int64 // unix ms
}

// NewEvent is an event to append (seq is assigned by AppendEvents).
type NewEvent struct {
	Kind string
	Data []byte
	TS   int64
}

// RunUpdate optionally changes a run's row in the same transaction as AppendEvents.
type RunUpdate struct {
	HermesSeq int64  // highest Hermes seq handled; ignored if < 0
	Status    string // new status; ignored if empty
}

// AppendEvents numbers and stores evs for a session (seq continues from the session's last one) and applies upd to
// the run, atomically. It returns the stored events.
func (s *Store) AppendEvents(ctx context.Context, sessionID, runID string, evs []NewEvent, upd RunUpdate) ([]Event, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := s.ms()
	out := make([]Event, 0, len(evs))
	if len(evs) > 0 {
		var last int64
		err := tx.QueryRowContext(ctx, `INSERT INTO sessions (session_id, last_seq) VALUES (?, ?)
			ON CONFLICT(session_id) DO UPDATE SET last_seq = last_seq + ? RETURNING last_seq`,
			sessionID, len(evs), len(evs)).Scan(&last)
		if err != nil {
			return nil, err
		}
		first := last - int64(len(evs)) + 1
		for i, e := range evs {
			ev := Event{SessionID: sessionID, Seq: first + int64(i), RunID: runID, Kind: e.Kind, Data: e.Data, TS: e.TS}
			if _, err := tx.ExecContext(ctx, `INSERT INTO events (session_id, seq, run_id, kind, data, ts, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, ev.SessionID, ev.Seq, ev.RunID, ev.Kind, string(ev.Data), ev.TS, now); err != nil {
				return nil, err
			}
			out = append(out, ev)
		}
	}
	if upd.HermesSeq >= 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET hermes_seq = MAX(hermes_seq, ?), updated_at = ? WHERE run_id = ?`,
			upd.HermesSeq, now, runID); err != nil {
			return nil, err
		}
	}
	if upd.Status != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET status = ?, updated_at = ? WHERE run_id = ?`, upd.Status, now, runID); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

// ErrReplayGap means the events after the requested seq are no longer (or were never) stored.
var ErrReplayGap = errors.New("replay gap")

// EventsAfter returns a session's events with seq > after, oldest first. It fails with ErrReplayGap when some of
// them were pruned, when after is beyond the last seq (the database was reset), or when there are more than limit.
func (s *Store) EventsAfter(ctx context.Context, sessionID string, after int64, limit int) ([]Event, error) {
	var last, pruned int64
	err := s.db.QueryRowContext(ctx, `SELECT last_seq, pruned_seq FROM sessions WHERE session_id = ?`, sessionID).Scan(&last, &pruned)
	if errors.Is(err, sql.ErrNoRows) {
		last, pruned = 0, 0
	} else if err != nil {
		return nil, err
	}
	if after < pruned || after > last || last-after > int64(limit) {
		return nil, ErrReplayGap
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq, run_id, kind, data, ts FROM events WHERE session_id = ? AND seq > ? ORDER BY seq`,
		sessionID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e := Event{SessionID: sessionID}
		var data string
		if err := rows.Scan(&e.Seq, &e.RunID, &e.Kind, &data, &e.TS); err != nil {
			return nil, err
		}
		e.Data = []byte(data)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if int64(len(out)) != last-after { // a hole: pruned under us
		return nil, ErrReplayGap
	}
	return out, nil
}

// LastEvent returns a run's newest stored event of kind, or ErrNotFound.
func (s *Store) LastEvent(ctx context.Context, sessionID, runID, kind string) (Event, error) {
	e := Event{SessionID: sessionID, RunID: runID, Kind: kind}
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT seq, data, ts FROM events WHERE session_id = ? AND run_id = ? AND kind = ?
		ORDER BY seq DESC LIMIT 1`, sessionID, runID, kind).Scan(&e.Seq, &data, &e.TS)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	} else if err != nil {
		return Event{}, err
	}
	e.Data = []byte(data)
	return e, nil
}

// Run is a Hermes run started through the relay.
type Run struct {
	RunID       string
	SessionID   string
	ClientMsgID string
	DeviceID    int64
	Status      string
	HermesSeq   int64
}

// InsertRun records a new run. A run that already exists is left as is.
func (s *Store) InsertRun(ctx context.Context, r Run) error {
	now := s.ms()
	var cmid any
	if r.ClientMsgID != "" {
		cmid = r.ClientMsgID
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs (run_id, session_id, client_msg_id, device_id, status, hermes_seq, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, -1, ?, ?) ON CONFLICT DO NOTHING`, r.RunID, r.SessionID, cmid, r.DeviceID, r.Status, now, now)
	return err
}

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var cmid sql.NullString
	var dev sql.NullInt64
	err := row.Scan(&r.RunID, &r.SessionID, &cmid, &dev, &r.Status, &r.HermesSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	r.ClientMsgID, r.DeviceID = cmid.String, dev.Int64
	return r, err
}

const runCols = `run_id, session_id, client_msg_id, device_id, status, hermes_seq`

// RunByClientMsgID finds the run a prompt started.
func (s *Store) RunByClientMsgID(ctx context.Context, clientMsgID string) (Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE client_msg_id = ?`, clientMsgID))
}

// RunByID finds a run.
func (s *Store) RunByID(ctx context.Context, runID string) (Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE run_id = ?`, runID))
}

// ActiveRuns lists runs not yet completed, failed or cancelled.
func (s *Store) ActiveRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs WHERE status NOT IN ('completed','failed','cancelled') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
