// Subscription rows: mute flags and enabled event types.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// LinkedServices returns the service ids a user is linked to (active).
func (s *Store) LinkedServices(ctx context.Context, userID int64) []string {
	rows, err := s.db.QueryContext(ctx,
		`SELECT service FROM service_users WHERE user_id=? AND status='active'`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var svc string
		if err := rows.Scan(&svc); err == nil {
			out = append(out, svc)
		}
	}
	return out
}

// SubscriptionMuted reports whether a user muted a service.
func (s *Store) SubscriptionMuted(ctx context.Context, userID int64, service string) (bool, error) {
	var m int
	err := s.db.QueryRowContext(ctx,
		`SELECT muted FROM subscriptions WHERE user_id=? AND service=?`, userID, service).Scan(&m)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return m == 1, err
}

// EventTypesEnabled returns the enabled event type list (nil = ALL, i.e. enabled).
func (s *Store) EventTypesEnabled(ctx context.Context, userID int64, service string) ([]string, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT event_types FROM subscriptions WHERE user_id=? AND service=?`, userID, service).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}
	var out []string
	_ = json.Unmarshal([]byte(raw.String), &out)
	return out, nil
}

// SetMuted sets the service-level mute flag.
func (s *Store) SetMuted(ctx context.Context, userID int64, service string, muted bool) error {
	v := 0
	if muted {
		v = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscriptions SET muted=?, updated_at=? WHERE user_id=? AND service=?`,
		v, time.Now().UTC().Format(time.RFC3339), userID, service)
	return err
}

// SetEventTypes persists the enabled event-types JSON (empty = ALL).
func (s *Store) SetEventTypes(ctx context.Context, userID int64, service string, types []string) error {
	var raw any
	if len(types) == 0 {
		raw = ""
	} else {
		b, _ := json.Marshal(types)
		raw = string(b)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscriptions SET event_types=?, updated_at=? WHERE user_id=? AND service=?`,
		raw, time.Now().UTC().Format(time.RFC3339), userID, service)
	return err
}
