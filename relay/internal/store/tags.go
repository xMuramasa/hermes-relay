package store

import "context"

// SessionTags returns every session's tags, in the order they were set.
func (s *Store) SessionTags(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, tag FROM session_tags ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var id, tag string
		if err := rows.Scan(&id, &tag); err != nil {
			return nil, err
		}
		out[id] = append(out[id], tag)
	}
	return out, rows.Err()
}

// SetSessionTags replaces a session's tags (none drops them). The caller checks and dedupes them.
func (s *Store) SetSessionTags(ctx context.Context, sessionID string, tags []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_tags WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	for _, t := range tags {
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_tags (session_id, tag) VALUES (?, ?)`, sessionID, t); err != nil {
			return err
		}
	}
	return tx.Commit()
}
