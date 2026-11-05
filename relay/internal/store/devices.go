package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"time"
)

// Device roles. A manager may use the management endpoints; chat, approvals and sessions are open to both.
const (
	RoleManager = "manager"
	RoleViewer  = "viewer"
)

// ValidRole reports whether r is a known role.
func ValidRole(r string) bool { return r == RoleManager || r == RoleViewer }

// Device is a paired phone. The token itself is never stored, only its SHA-256.
type Device struct {
	ID         int64
	Name       string
	Role       string
	Terminal   bool // the grant; a terminal also needs the manager role
	CreatedAt  time.Time
	LastSeenAt time.Time // zero if never seen
}

func msTime(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.UnixMilli(v.Int64)
}

// CreatePairCode is CreatePairCodeWithRole for a viewer.
func (s *Store) CreatePairCode(ctx context.Context, name string, codeHash []byte, ttl time.Duration, replace bool, maxDevices int) (revoked bool, err error) {
	return s.CreatePairCodeWithRole(ctx, name, RoleViewer, codeHash, ttl, replace, maxDevices)
}

// CreatePairCodeWithRole stores a one-time code (hashed) for a new device called name with the given role. If a
// device with that name exists, it fails with ErrNameExists unless replace is set, in which case the old device is
// revoked. Other unused codes for the same name are dropped.
func (s *Store) CreatePairCodeWithRole(ctx context.Context, name, role string, codeHash []byte, ttl time.Duration, replace bool, maxDevices int) (revoked bool, err error) {
	if !ValidRole(role) {
		return false, ErrInvalidRole
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM devices WHERE name = ?`, name).Scan(&id)
	switch {
	case err == nil && !replace:
		return false, ErrNameExists
	case err == nil:
		if _, err := tx.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, id); err != nil {
			return false, err
		}
		revoked = true
	case !errors.Is(err, sql.ErrNoRows):
		return false, err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices`).Scan(&n); err != nil {
		return false, err
	}
	if n >= maxDevices {
		return false, ErrTooManyDevices
	}
	now := s.ms()
	if _, err := tx.ExecContext(ctx, `DELETE FROM pair_codes WHERE name = ? AND used_at IS NULL`, name); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pair_codes (code_hash, name, role, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		codeHash, name, role, now, now+ttl.Milliseconds()); err != nil {
		return false, err
	}
	return revoked, tx.Commit()
}

// RedeemPairCode trades a valid code for a new device with the given token hash. The code is used up even if the
// device can't be created (name taken meanwhile, limit reached).
func (s *Store) RedeemPairCode(ctx context.Context, codeHash, tokenHash []byte, maxDevices int) (Device, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Device{}, err
	}
	defer tx.Rollback()
	now := s.ms()
	var name, role string
	err = tx.QueryRowContext(ctx, `SELECT name, role FROM pair_codes WHERE code_hash = ? AND used_at IS NULL AND expires_at > ?`,
		codeHash, now).Scan(&name, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrInvalidCode
	} else if err != nil {
		return Device{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pair_codes SET used_at = ? WHERE code_hash = ?`, now, codeHash); err != nil {
		return Device{}, err
	}
	var n, taken int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(name = ?), 0) FROM devices`, name).Scan(&n, &taken); err != nil {
		return Device{}, err
	}
	var failure error
	switch {
	case taken > 0:
		failure = ErrNameExists
	case n >= maxDevices:
		failure = ErrTooManyDevices
	}
	if failure != nil {
		if err := tx.Commit(); err != nil { // keep the code used
			return Device{}, err
		}
		return Device{}, failure
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO devices (name, role, token_hash, created_at) VALUES (?, ?, ?, ?)`, name, role, tokenHash, now)
	if err != nil {
		return Device{}, err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return Device{}, err
	}
	return Device{ID: id, Name: name, Role: role, CreatedAt: time.UnixMilli(now)}, nil
}

// DeviceByTokenHash finds the device owning a token.
func (s *Store) DeviceByTokenHash(ctx context.Context, tokenHash []byte) (Device, error) {
	var d Device
	var created int64
	var seen sql.NullInt64
	var stored []byte
	err := s.db.QueryRowContext(ctx, `SELECT id, name, role, terminal, token_hash, created_at, last_seen_at FROM devices WHERE token_hash = ?`,
		tokenHash).Scan(&d.ID, &d.Name, &d.Role, &d.Terminal, &stored, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	} else if err != nil {
		return Device{}, err
	}
	if subtle.ConstantTimeCompare(stored, tokenHash) != 1 {
		return Device{}, ErrNotFound
	}
	d.CreatedAt, d.LastSeenAt = time.UnixMilli(created), msTime(seen)
	return d, nil
}

// TouchDevice records that a device was seen (at most once a minute).
func (s *Store) TouchDevice(ctx context.Context, id int64) error {
	now := s.ms()
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET last_seen_at = ? WHERE id = ? AND (last_seen_at IS NULL OR last_seen_at < ?)`,
		now, id, now-60_000)
	return err
}

// ListDevices returns all devices by name.
func (s *Store) ListDevices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, role, terminal, created_at, last_seen_at FROM devices ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		var created int64
		var seen sql.NullInt64
		if err := rows.Scan(&d.ID, &d.Name, &d.Role, &d.Terminal, &created, &seen); err != nil {
			return nil, err
		}
		d.CreatedAt, d.LastSeenAt = time.UnixMilli(created), msTime(seen)
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeviceIDs returns the ids of all current devices (the server uses it to drop revoked devices' sockets).
func (s *Store) DeviceIDs(ctx context.Context) (map[int64]bool, error) {
	return s.deviceIDs(ctx, `SELECT id FROM devices`)
}

// TerminalDevices returns the ids of the devices allowed a terminal now: managers with the grant (the server uses it
// to close the terminals of devices revoked, demoted or without the grant).
func (s *Store) TerminalDevices(ctx context.Context) (map[int64]bool, error) {
	return s.deviceIDs(ctx, `SELECT id FROM devices WHERE terminal = 1 AND role = 'manager'`)
}

func (s *Store) deviceIDs(ctx context.Context, query string) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// RevokeDevice deletes a device by name.
func (s *Store) RevokeDevice(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDeviceTerminal turns a device's terminal grant on or off by name. The server closes open terminals at its next check.
func (s *Store) SetDeviceTerminal(ctx context.Context, name string, on bool) error {
	// Only a manager gets the grant (ErrNotManager for a viewer): a shell is never a dormant right waiting for a
	// promotion. Turning it off works on any device.
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET terminal = ? WHERE name = ? AND (? = 0 OR role = 'manager')`, on, name, on)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var role string
		if err := s.db.QueryRowContext(ctx, `SELECT role FROM devices WHERE name = ?`, name).Scan(&role); err != nil {
			return ErrNotFound
		}
		return ErrNotManager
	}
	return nil
}

// SetDeviceRole changes a device's role by name. It applies to the device's next request.
func (s *Store) SetDeviceRole(ctx context.Context, name, role string) error {
	if !ValidRole(role) {
		return ErrInvalidRole
	}
	// A demotion also drops the terminal grant: promoted again, the device needs a new `devices terminal NAME on`.
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET role = ?, terminal = CASE WHEN ? = 'manager' THEN terminal ELSE 0 END WHERE name = ?`, role, role, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
