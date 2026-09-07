// Link codes and connect codes (single-use, expiring).
package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// --- codes (connect) ---

// SetTopic stores/persists the topic id for (user,service). Test-only helper
// (SPEC t_2d992300 R5): production paths persist topics exclusively through
// EnsureTopic; this direct setter exists for the itest fixtures.
func (s *Store) SetTopic(ctx context.Context, userID int64, service string, threadID int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO group_topics (user_id, service, message_thread_id, created_at)
		VALUES (?,?,?,?) ON CONFLICT(user_id, service) DO UPDATE SET message_thread_id=excluded.message_thread_id`,
		userID, service, threadID, time.Now().UTC().Format(time.RFC3339))
	return err
}

// CreateLinkCode stores a link code for a service.
func (s *Store) CreateLinkCode(ctx context.Context, code, service string, userID int64, ttl time.Duration, _ any) error {
	exp := time.Now().UTC().Add(ttl).Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO link_codes (code, service, user_id, expires_at) VALUES (?,?,?,?)`, code, service, userID, exp)
	return err
}

// CreateConnectCode stores a new connect code for a user.
func (s *Store) CreateConnectCode(ctx context.Context, code string, userID int64, ttl time.Duration) error {
	exp := time.Now().UTC().Add(ttl).Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO connect_codes (code, user_id, expires_at) VALUES (?,?,?)`, code, userID, exp)
	return err
}

// ConsumeConnectCode validates + consumes a connect code atomically.
func (s *Store) ConsumeConnectCode(ctx context.Context, code string, userID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx,
		`UPDATE connect_codes SET used_at=? WHERE code=? AND used_at IS NULL AND expires_at > ? AND user_id=?`,
		now, code, now, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		var used sql.NullString
		er := tx.QueryRowContext(ctx, `SELECT used_at FROM connect_codes WHERE code=?`, code).Scan(&used)
		if er == nil && used.Valid {
			return ErrCodeUsed
		}
		return ErrCodeInvalid
	}
	return tx.Commit()
}

// UserIDForLinkCode returns the tg user who owns a link code (or ErrCodeInvalid).
func (s *Store) UserIDForLinkCode(ctx context.Context, code string) (int64, error) {
	var uid int64
	var exp, used sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id, expires_at, used_at FROM link_codes WHERE code=?`, code).Scan(&uid, &exp, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrCodeInvalid
	}
	if err != nil {
		return 0, err
	}
	if used.Valid {
		return 0, ErrCodeUsed
	}
	if t, perr := time.Parse(time.RFC3339, exp.String); perr == nil && time.Now().After(t) {
		return 0, ErrCodeInvalid
	}
	return uid, nil
}
