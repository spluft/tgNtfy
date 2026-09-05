// Ingested event rows and retention pruning.
package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// --- Events ---

// Event is a parsed, validated ingest event.
type Event struct {
	ID         int64
	EventID    string
	Service    string
	UserRef    string
	Type       string
	Severity   string
	Title      string
	Text       string
	URL        string
	Metadata   string
	ReceivedAt time.Time
}

// InsertEvent stores an event; returns ErrDuplicateEvent if the event_id already exists.
func (s *Store) InsertEvent(ctx context.Context, e *Event) error {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO events (event_id, service, user_ref, type, severity, title, text, url, metadata, received_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		e.EventID, e.Service, e.UserRef, e.Type, e.Severity, e.Title, e.Text, e.URL, e.Metadata,
		e.ReceivedAt.UTC().Format(time.RFC3339))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrDuplicateEvent
		}
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		e.ID = id
	}
	return nil
}

// LastEventForUserService returns the last event title + time for a user's service.
func (s *Store) LastEventForUserService(ctx context.Context, userID int64, service string) (string, time.Time, error) {
	var title, ra string
	err := s.db.QueryRowContext(ctx, `
		SELECT e.title, e.received_at FROM events e
		JOIN deliveries d ON d.event_id = e.event_id AND d.user_id = ?
		WHERE e.service=? ORDER BY d.id DESC LIMIT 1`, userID, service).Scan(&title, &ra)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, err
	}
	t, _ := time.Parse(time.RFC3339, ra)
	return title, t, nil
}
