package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// UnclaimedTTL is how long an upload no prompt used is kept.
const UnclaimedTTL = time.Hour

// Attachment is an uploaded image; its id is the SHA-256 of its bytes, in hex.
type Attachment struct {
	ID     string
	Mime   string
	Bytes  int64
	Width  int
	Height int
}

var attachmentIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidAttachmentID reports whether id is a lowercase hex SHA-256 (so it is also a safe file name).
func ValidAttachmentID(id string) bool { return attachmentIDRe.MatchString(id) }

func (s *Store) attachmentPath(id string) (string, error) {
	if !ValidAttachmentID(id) {
		return "", ErrNotFound
	}
	return filepath.Join(s.dir, id), nil
}

// PutAttachment stores an upload (file first, then its row). created is false when the same bytes were already
// there; an unclaimed one then gets a fresh hour.
func (s *Store) PutAttachment(ctx context.Context, a Attachment, data []byte) (created bool, err error) {
	path, err := s.attachmentPath(a.ID)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); err != nil {
		if err := os.MkdirAll(s.dir, 0o700); err != nil {
			return false, err
		}
		f, err := os.CreateTemp(s.dir, ".upload-*")
		if err != nil {
			return false, err
		}
		_, werr := f.Write(data)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr == nil {
			werr = os.Rename(f.Name(), path)
		}
		if werr != nil {
			_ = os.Remove(f.Name())
			return false, werr
		}
	}
	now := s.ms()
	res, err := s.db.ExecContext(ctx, `INSERT INTO attachments (id, mime, bytes, width, height, created_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, a.ID, a.Mime, a.Bytes, a.Width, a.Height, now)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true, nil
	}
	_, err = s.db.ExecContext(ctx, `UPDATE attachments SET created_at = ? WHERE id = ? AND claimed_at IS NULL`, now, a.ID)
	return false, err
}

// Attachments returns the attachments with these ids, in order, or ErrNotFound if any is unknown.
func (s *Store) Attachments(ctx context.Context, ids []string) ([]Attachment, error) {
	out := make([]Attachment, 0, len(ids))
	for _, id := range ids {
		a := Attachment{ID: id}
		err := s.db.QueryRowContext(ctx, `SELECT mime, bytes, width, height FROM attachments WHERE id = ?`, id).
			Scan(&a.Mime, &a.Bytes, &a.Width, &a.Height)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		} else if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// OpenAttachment opens an attachment's file.
func (s *Store) OpenAttachment(id string) (*os.File, error) {
	path, err := s.attachmentPath(id)
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}

// ReadAttachment reads an attachment's bytes.
func (s *Store) ReadAttachment(id string) ([]byte, error) {
	path, err := s.attachmentPath(id)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// ImageRun is a relay run that carried images, for matching it to the session history.
type ImageRun struct {
	RunID       string
	CreatedAt   int64 // unix ms
	Attachments []Attachment
}

// SessionImageRuns lists a session's runs with images, oldest first, each with its images in prompt order.
func (s *Store) SessionImageRuns(ctx context.Context, sessionID string) ([]ImageRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.run_id, r.created_at, a.id, a.mime, a.bytes, a.width, a.height
		FROM runs r JOIN run_attachments ra ON ra.run_id = r.run_id JOIN attachments a ON a.id = ra.attachment_id
		WHERE r.session_id = ? ORDER BY r.created_at, r.run_id, ra.idx`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImageRun
	for rows.Next() {
		var r ImageRun
		var a Attachment
		if err := rows.Scan(&r.RunID, &r.CreatedAt, &a.ID, &a.Mime, &a.Bytes, &a.Width, &a.Height); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].RunID == r.RunID {
			out[n-1].Attachments = append(out[n-1].Attachments, a)
			continue
		}
		r.Attachments = []Attachment{a}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DropSessionAttachments unlinks a deleted session's images and deletes those no other run uses.
func (s *Store) DropSessionAttachments(ctx context.Context, sessionID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT attachment_id FROM run_attachments
		WHERE run_id IN (SELECT run_id FROM runs WHERE session_id = ?)`, sessionID)
	if err != nil {
		return err
	}
	ids, err := scanIDs(rows)
	if err != nil || len(ids) == 0 {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM run_attachments WHERE run_id IN (SELECT run_id FROM runs WHERE session_id = ?)`,
		sessionID); err != nil {
		return err
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	gone, err := deleteAttachments(ctx, tx, `SELECT id FROM attachments WHERE id IN (?`+strings.Repeat(",?", len(ids)-1)+`)
		AND id NOT IN (SELECT attachment_id FROM run_attachments)`, args...)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.removeFiles(gone)
	return nil
}

// deleteAttachments deletes the attachment rows query selects and returns their ids (remove the files after commit).
func deleteAttachments(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	ids, err := scanIDs(rows)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM attachments WHERE id = ?`, id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func scanIDs(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) removeFiles(ids []string) {
	for _, id := range ids {
		if path, err := s.attachmentPath(id); err == nil {
			_ = os.Remove(path)
		}
	}
}
