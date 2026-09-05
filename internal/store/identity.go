// Identity linking (link-code redemption) in transactions.
package store

import (
	"context"
	"database/sql"
	"time"
)

// --- service_users + subscriptions ---

// execer is satisfied by both *sql.DB and *sql.Tx so the display_name UPDATE runs inside
// the LinkIdentity transaction or standalone.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// setServiceDisplayNameIn updates services.display_name inside the caller's tx (V-2).
func setServiceDisplayNameIn(e execer, ctx context.Context, service, name string) error {
	_, err := e.ExecContext(ctx,
		`UPDATE services SET display_name=? WHERE service=?`, name, service)
	return err
}

// LinkIdentity binds (service,user_ref) to a TG user and ensures a subscription row,
// atomically consuming a link code (UC-2). Returns ErrDuplicateEvent for constraint
// failures and consumes the code ONLY within the same transaction.
//
// displayName is OPTIONAL (V-2): when non-empty it updates services.display_name inside the
// same transaction (the service self-names at link); when empty the existing name is kept.
func (s *Store) LinkIdentity(ctx context.Context, service, userRef, code string, codeUserID, tgUserID int64, displayName string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)

	// Consume the code atomically (single use).
	res, err := tx.ExecContext(ctx,
		`UPDATE link_codes SET used_at=? WHERE code=? AND used_at IS NULL AND expires_at > ? AND user_id=?`,
		now, code, now, codeUserID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Distinguish expired/invalid vs already-used.
		var used sql.NullString
		var svc string
		var uid int64
		er := tx.QueryRowContext(ctx,
			`SELECT used_at, service, user_id FROM link_codes WHERE code=?`, code).Scan(&used, &svc, &uid)
		if er == nil {
			if used.Valid {
				return ErrCodeUsed
			}
			if svc != service {
				return ErrCodeMismatch
			}
		}
		return ErrCodeInvalid
	}

	// Enforce "one service identity belongs to one TG user".
	if codeUserID != tgUserID {
		return ErrCodeMismatch
	}

	// Upsert service_users: if bound to a DIFFERENT user -> already_linked.
	var existing sql.NullInt64
	er := tx.QueryRowContext(ctx,
		`SELECT user_id FROM service_users WHERE service=? AND user_ref=?`, service, userRef).Scan(&existing)
	if er == nil {
		if existing.Valid && existing.Int64 != tgUserID {
			return ErrAlreadyLinked
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO service_users (service, user_ref, user_id, linked_at)
		VALUES (?,?,?,?) ON CONFLICT(service,user_ref)
		DO UPDATE SET status='active'`,
		service, userRef, tgUserID, now)
	if err != nil {
		return err
	}

	// Ensure a subscription row with all event types on.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO subscriptions (user_id, service, updated_at) VALUES (?,?,?)
		ON CONFLICT(user_id, service) DO UPDATE SET muted=0`,
		tgUserID, service, now)
	if err != nil {
		return err
	}
	if displayName != "" {
		if err := setServiceDisplayNameIn(tx, ctx, service, displayName); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// EnsureServiceUser ensures a service_user + subscription row exist for admin-created paths.
func (s *Store) EnsureServiceUser(ctx context.Context, service, userRef string, userID int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO service_users (service, user_ref, user_id, linked_at)
		VALUES (?,?,?,?) ON CONFLICT(service,user_ref) DO UPDATE SET status='active'`,
		service, userRef, userID, now)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO subscriptions (user_id, service, updated_at) VALUES (?,?,?)
		ON CONFLICT(user_id, service) DO NOTHING`, userID, service, now)
	return err
}
