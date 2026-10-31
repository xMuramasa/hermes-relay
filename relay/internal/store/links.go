package store

import (
	"context"
	"errors"
)

// Link errors.
var (
	ErrLinkTaken     = errors.New("that conversation is already continued by another one")
	ErrLinkCycle     = errors.New("that link would make a loop")
	ErrChainTooLong  = errors.New("that chain would be too long")
	errLinkCorrupted = errors.New("session_links holds a loop")
)

// SessionLinks returns every handoff link, newer id → the older id it continues.
func (s *Store) SessionLinks(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, continues FROM session_links`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var newer, older string
		if err := rows.Scan(&newer, &older); err != nil {
			return nil, err
		}
		out[newer] = older
	}
	return out, rows.Err()
}

// SetContinues makes newer continue older (replacing newer's link), or drops newer's link when older is "".
// The links stay straight chains of at most maxLinks links.
func (s *Store) SetContinues(ctx context.Context, newer, older string, maxLinks int) error {
	if older == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM session_links WHERE session_id = ?`, newer)
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT session_id, continues FROM session_links WHERE session_id <> ?`, newer)
	if err != nil {
		return err
	}
	links, next := map[string]string{}, map[string]string{} // newer → older, older → newer
	for rows.Next() {
		var n, o string
		if err := rows.Scan(&n, &o); err != nil {
			rows.Close()
			return err
		}
		links[n], next[o] = o, n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if _, ok := next[older]; ok {
		return ErrLinkTaken
	}
	n := 1 // links in the chain once this one is in
	for o, ok := links[older]; ok; o, ok = links[o] {
		if o == newer {
			return ErrLinkCycle
		}
		if n++; n > len(links)+1 {
			return errLinkCorrupted
		}
	}
	for c, ok := next[newer]; ok; c, ok = next[c] {
		if n++; n > len(links)+1 {
			return errLinkCorrupted
		}
	}
	if n > maxLinks {
		return ErrChainTooLong
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_links (session_id, continues) VALUES (?, ?)
		ON CONFLICT (session_id) DO UPDATE SET continues = excluded.continues`, newer, older); err != nil {
		return err
	}
	return tx.Commit()
}

// SessionModel is a model chosen for a session.
type SessionModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// SessionModels returns every session's chosen model.
func (s *Store) SessionModels(ctx context.Context) (map[string]SessionModel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, provider, model FROM session_models`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SessionModel{}
	for rows.Next() {
		var id string
		var m SessionModel
		if err := rows.Scan(&id, &m.Provider, &m.Model); err != nil {
			return nil, err
		}
		out[id] = m
	}
	return out, rows.Err()
}

// SetSessionModel records a session's chosen model.
func (s *Store) SetSessionModel(ctx context.Context, sessionID string, m SessionModel) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO session_models (session_id, provider, model) VALUES (?, ?, ?)
		ON CONFLICT (session_id) DO UPDATE SET provider = excluded.provider, model = excluded.model`,
		sessionID, m.Provider, m.Model)
	return err
}

// ForgetSession drops a deleted session's links (both sides, leaving two shorter chains) and model choice.
func (s *Store) ForgetSession(ctx context.Context, sessionID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_links WHERE session_id = ?1 OR continues = ?1`, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_models WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}
