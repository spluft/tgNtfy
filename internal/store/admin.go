// Admin CLI read/write helpers (no HTTP surface).
package store

import (
	"context"
	"database/sql"
	"time"
)

// AdminLink binds a service user_ref to a tg user id without a code (admin CLI).
func (s *Store) AdminLink(ctx context.Context, service, userRef string, tgUserID int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO service_users (service, user_ref, user_id, linked_at)
		VALUES (?,?,?,?) ON CONFLICT(service, user_ref) DO UPDATE SET user_id=excluded.user_id, status='active'`,
		service, userRef, tgUserID, now)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO subscriptions (user_id, service, updated_at) VALUES (?,?,?)
		ON CONFLICT(user_id, service) DO NOTHING`, tgUserID, service, now)
	return err
}

// AdminUnlink marks a service_user unlinked.
func (s *Store) AdminUnlink(ctx context.Context, service, userRef string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE service_users SET status='unlinked' WHERE service=? AND user_ref=?`, service, userRef)
	return err
}

// UserList lists users for admin.
type UserInfo struct {
	TgUserID     int64
	Username     string
	DeliveryMode string
	GroupChatID  *int64
}

func (s *Store) UserList(ctx context.Context) ([]UserInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tg_user_id, username, delivery_mode, group_chat_id FROM users ORDER BY tg_user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserInfo
	for rows.Next() {
		var u UserInfo
		var un sql.NullString
		var gci sql.NullInt64
		if err := rows.Scan(&u.TgUserID, &un, &u.DeliveryMode, &gci); err != nil {
			return nil, err
		}
		u.Username = un.String
		if gci.Valid {
			v := gci.Int64
			u.GroupChatID = &v
		}
		out = append(out, u)
	}
	return out, nil
}

// RecentEvents lists recent events for admin audit.
type RecentEvent struct {
	EventID  string
	Service  string
	UserRef  string
	Type     string
	Title    string
	Received time.Time
}

func (s *Store) RecentEvents(ctx context.Context, n int) ([]RecentEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT event_id, service, user_ref, type, title, received_at FROM events ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecentEvent
	for rows.Next() {
		var r RecentEvent
		var ra string
		if err := rows.Scan(&r.EventID, &r.Service, &r.UserRef, &r.Type, &r.Title, &ra); err != nil {
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339, ra)
		r.Received = t
		out = append(out, r)
	}
	return out, nil
}
