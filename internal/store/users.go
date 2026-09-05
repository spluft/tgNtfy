// User rows and delivery-mode settings.
package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// --- Users ---

// UpsertUser creates or updates a user row from a TG update.
func (s *Store) UpsertUser(ctx context.Context, tgUserID int64, username, firstName string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (tg_user_id, username, first_name, first_seen)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(tg_user_id) DO UPDATE SET username=excluded.username, first_name=excluded.first_name`,
		tgUserID, nullStr(username), nullStr(firstName), now)
	return err
}

// UserRow is a user.
type UserRow struct {
	TgUserID     int64
	Username     string
	FirstName    string
	DeliveryMode string
	GroupChatID  *int64
}

// GetUser returns a user or (nil, nil) if absent.
func (s *Store) GetUser(ctx context.Context, tgUserID int64) (*UserRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tg_user_id, username, first_name, delivery_mode, group_chat_id FROM users WHERE tg_user_id = ?`, tgUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u UserRow
		var un, fn sql.NullString
		var gci sql.NullInt64
		if err := rows.Scan(&u.TgUserID, &un, &fn, &u.DeliveryMode, &gci); err != nil {
			return nil, err
		}
		u.Username, u.FirstName = un.String, fn.String
		if gci.Valid {
			v := gci.Int64
			u.GroupChatID = &v
		}
		return &u, nil
	}
	return nil, rows.Err()
}

// ClaimGovpnAdmin auto-links govpn user_ref 'admin' to the first /start user (UC-2 2a).
func (s *Store) ClaimGovpnAdmin(ctx context.Context, tgUserID int64) error {
	var linked sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id FROM service_users WHERE service='govpn' AND user_ref='admin'`).Scan(&linked)
	if err == nil {
		return nil // already claimed
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO service_users (service, user_ref, user_id, linked_at)
		VALUES ('govpn','admin',?,?)`,
		tgUserID, time.Now().UTC().Format(time.RFC3339))
	return err
}

// SetDeliveryMode sets a user's delivery mode + group chat id.
func (s *Store) SetDeliveryMode(ctx context.Context, tgUserID int64, mode string, groupChatID *int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET delivery_mode=?, group_chat_id=? WHERE tg_user_id=?`,
		mode, groupChatID, tgUserID)
	return err
}
