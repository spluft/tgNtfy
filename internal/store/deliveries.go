// Delivery rows: the pending/sent/failed lifecycle + bookkeeping.
package store

import (
	"context"
	"time"
)

// --- deliveries ---
// Delivery is a delivery row.
type Delivery struct {
	ID        int64
	UserID    int64
	EventID   string
	Service   string
	Type      string
	BatchSize int
	TgMsgID   int64
	ThreadID  int
	Status    string
	Attempts  int
	LastErr   string
	ChatID    int64
	Text      string
}

// MarkDeliverySent records a successful TG send.
func (s *Store) MarkDeliverySent(ctx context.Context, id, tgMsgID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE deliveries SET status='sent', tg_msg_id=?, attempts=attempts+1, next_retry_at=NULL, last_err=NULL, updated_at=?
		 WHERE id=?`,
		tgMsgID, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// MarkDeliveryFailed records a failed attempt and schedules the next retry.
func (s *Store) MarkDeliveryFailed(ctx context.Context, id int64, attempts int, nextRetry *time.Time, errMsg string) error {
	var nr any
	if nextRetry != nil {
		nr = nextRetry.UTC().Format(time.RFC3339)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE deliveries SET status='failed', attempts=?, next_retry_at=?, last_err=?, updated_at=? WHERE id=?`,
		attempts, nr, errMsg, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// FailDeliveryPermanently marks a delivery failed (retries exhausted).
func (s *Store) FailDeliveryPermanently(ctx context.Context, id int64, attempts int, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE deliveries SET status='failed', attempts=?, next_retry_at=NULL, last_err=?, updated_at=? WHERE id=?`,
		attempts, errMsg, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// RetryAllFailed resets failed deliveries to pending (bounded to max).
func (s *Store) RetryAllFailed(ctx context.Context, max int) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE deliveries SET status='pending', attempts=0, next_retry_at=NULL, updated_at=?
		WHERE id IN (SELECT id FROM deliveries WHERE status='failed' ORDER BY id LIMIT ?)`,
		time.Now().UTC().Format(time.RFC3339), max)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// FailedDeliveries lists up to limit failed deliveries (for /status and /undelivered).
func (s *Store) FailedDeliveries(ctx context.Context, limit int) ([]*Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, event_id, service, type, batch_size, thread_id, status, attempts, last_err
		FROM deliveries WHERE status='failed' ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Delivery
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.ID, &d.UserID, &d.EventID, &d.Service, &d.Type, &d.BatchSize, &d.ThreadID, &d.Status, &d.Attempts, &d.LastErr); err != nil {
			return nil, err
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

// CountFailed counts failed deliveries for a user.
func (s *Store) CountFailed(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM deliveries WHERE user_id=? AND status='failed'`, userID).Scan(&n)
	return n, err
}

// DeliveryRetry429 records a TG 429 deferral (attempts unchanged).
func (s *Store) DeliveryRetry429(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE deliveries SET last_err='flood limit', updated_at=? WHERE id=?`,
		time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// CreateDelivery inserts a delivery row (at coalesce flush) and returns its id.
func (s *Store) CreateDelivery(ctx context.Context, userID, chatID int64, threadID int, eventID, service, typ string, batchSize int) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO deliveries (user_id, event_id, service, type, batch_size, thread_id, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?, 'pending', ?, ?)`,
		userID, eventID, service, typ, batchSize, threadID, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// DeliveryBatchStats returns SUM and MAX of batch_size over a (user,service)'s
// delivery rows. Read helper for the coalescing QA oracle (SPEC t_2d992300
// R5: the aggregate query moved here from the itest raw-SQL passthrough —
// no SQL lives outside this package).
func (s *Store) DeliveryBatchStats(ctx context.Context, userID int64, service string) (sum, maxBatch int, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(batch_size),0), COALESCE(MAX(batch_size),0) FROM deliveries WHERE user_id=? AND service=?`,
		userID, service).Scan(&sum, &maxBatch)
	return sum, maxBatch, err
}
