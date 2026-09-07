// group_topics rows: the (user,service) -> message_thread_id map.
package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// --- group_topics ---

// EnsureTopic returns the idempotent topic id for (user,service) (D-IDEM-1): if a
// group_topics row already exists it is reused (created=false, no Bot API call); else the
// creator callback creates the forum topic and the row is upserted with ON CONFLICT DO
// NOTHING (a concurrent racer may win — its thread id wins, ours is an orphan, tolerated).
// creator is injected so the store never imports tgbot.
func (s *Store) EnsureTopic(ctx context.Context, userID, chatID int64, service string, creator func(chatID int64, service string) (int, error)) (threadID int, created bool, err error) {
	{
		var tid int
		err := s.db.QueryRowContext(ctx,
			`SELECT message_thread_id FROM group_topics WHERE user_id=? AND service=?`, userID, service).Scan(&tid)
		if err == nil {
			return tid, false, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, err
		}
	}
	tid, err := creator(chatID, service)
	if err != nil {
		return 0, false, err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO group_topics (user_id, service, message_thread_id, created_at)
		VALUES (?,?,?,?) ON CONFLICT(user_id, service) DO NOTHING`,
		userID, service, tid, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, true, err
	}
	return tid, true, nil
}

// DisplayName returns a service's display name, falling back to the service id when the
// service row does not exist (safe for the resolver/renderer when no catalog is present).
func (s *Store) DisplayName(ctx context.Context, service string) (string, error) {
	var dn string
	err := s.db.QueryRowContext(ctx, `SELECT display_name FROM services WHERE service=?`, service).Scan(&dn)
	if errors.Is(err, sql.ErrNoRows) {
		return service, nil
	}
	if err != nil {
		return service, err
	}
	if dn == "" {
		return service, nil
	}
	return dn, nil
}

// GetTopicThread returns the stored message_thread_id for (user,service).
// Same query + scan semantics the coalesce flusher used via the raw-SQL
// QueryRow passthrough: on miss or any scan error the thread is 0 (found=false).
func (s *Store) GetTopicThread(ctx context.Context, userID int64, service string) (threadID int, found bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT message_thread_id FROM group_topics WHERE user_id=? AND service=?`, userID, service).Scan(&threadID)
	if err != nil {
		return 0, false, err
	}
	return threadID, true, nil
}

// ClearUserTopics deletes all of a user's group_topics rows (V-9): a new group bind
// means old message_thread_ids point at the previous chat and would 403 on delivery.
func (s *Store) ClearUserTopics(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM group_topics WHERE user_id=?`, userID)
	return err
}
