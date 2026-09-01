// Package store is the single owner of SQLite persistence. It uses modernc.org/sqlite
// (pure Go, no cgo). All events are persisted here; a single writer path (via the
// shared *sql.DB which serializes writes) plus pragmatic readers. PRAGMAs follow the
// SPEC: WAL, busy_timeout, synchronous=NORMAL, foreign_keys=ON.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Well-known sentinel errors.
var (
	ErrDuplicateEvent = errors.New("duplicate event_id within retention window")
	ErrCodeInvalid    = errors.New("code not found or expired")
	ErrCodeUsed       = errors.New("code already used")
	ErrCodeMismatch   = errors.New("code bound to a different service")
	ErrAlreadyLinked  = errors.New("service user_ref already linked to another user")
	ErrUnknown        = errors.New("not found")
)

// Store wraps the SQLite database handle.
type Store struct {
	db *sql.DB
}

// New opens (creating if needed) the SQLite file at path and runs the DDL.
func New(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite %s: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// dbSchema is the frozen DDL from SPEC §6.
const dbSchema = `
CREATE TABLE IF NOT EXISTS users (
  tg_user_id     INTEGER PRIMARY KEY,
  username       TEXT,
  first_name     TEXT,
  delivery_mode  TEXT NOT NULL DEFAULT 'dm' CHECK (delivery_mode IN ('dm','group')),
  group_chat_id  INTEGER,
  first_seen     TEXT NOT NULL,
  last_event_at  TEXT
);
CREATE TABLE IF NOT EXISTS services (
  service       TEXT PRIMARY KEY,
  display_name  TEXT NOT NULL,
  token_hash    TEXT NOT NULL,
  enabled       INTEGER NOT NULL DEFAULT 1,
  created_at    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS service_users (
  service    TEXT NOT NULL REFERENCES services(service),
  user_ref   TEXT NOT NULL,
  user_id    INTEGER NOT NULL REFERENCES users(tg_user_id),
  status     TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','unlinked')),
  linked_at  TEXT NOT NULL,
  PRIMARY KEY (service, user_ref)
);
CREATE TABLE IF NOT EXISTS subscriptions (
  user_id      INTEGER NOT NULL REFERENCES users(tg_user_id),
  service      TEXT NOT NULL REFERENCES services(service),
  event_types  TEXT,
  muted        INTEGER NOT NULL DEFAULT 0,
  updated_at   TEXT NOT NULL,
  PRIMARY KEY (user_id, service)
);
CREATE TABLE IF NOT EXISTS events (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id     TEXT NOT NULL,
  service      TEXT NOT NULL,
  user_ref     TEXT NOT NULL,
  type         TEXT NOT NULL,
  severity     TEXT NOT NULL CHECK (severity IN ('info','warn','error','success')),
  title        TEXT NOT NULL,
  text         TEXT NOT NULL DEFAULT '',
  url          TEXT NOT NULL DEFAULT '',
  metadata     TEXT NOT NULL DEFAULT '{}',
  received_at  TEXT NOT NULL,
  UNIQUE (event_id)
);
CREATE INDEX IF NOT EXISTS idx_events_service_time ON events(service, received_at);
CREATE TABLE IF NOT EXISTS deliveries (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id      INTEGER NOT NULL REFERENCES users(tg_user_id),
  event_id     TEXT NOT NULL,
  service      TEXT NOT NULL,
  type         TEXT NOT NULL,
  batch_size   INTEGER NOT NULL DEFAULT 1,
  tg_msg_id    INTEGER,
  thread_id    INTEGER,
  status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','sent','delivered','failed')),
  attempts     INTEGER NOT NULL DEFAULT 0,
  next_retry_at TEXT,
  last_err     TEXT,
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_deliveries_user_status ON deliveries(user_id, status);
CREATE INDEX IF NOT EXISTS idx_deliveries_retry ON deliveries(status, next_retry_at);
CREATE TABLE IF NOT EXISTS group_topics (
  user_id            INTEGER NOT NULL REFERENCES users(tg_user_id),
  service            TEXT NOT NULL REFERENCES services(service),
  message_thread_id  INTEGER NOT NULL,
  created_at         TEXT NOT NULL,
  PRIMARY KEY (user_id, service)
);
CREATE TABLE IF NOT EXISTS link_codes (
  code       TEXT PRIMARY KEY,
  service    TEXT NOT NULL REFERENCES services(service),
  user_id    INTEGER NOT NULL REFERENCES users(tg_user_id),
  expires_at TEXT NOT NULL,
  used_at    TEXT
);
CREATE TABLE IF NOT EXISTS connect_codes (
  code       TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(tg_user_id),
  expires_at TEXT NOT NULL,
  used_at    TEXT
);
`

func (s *Store) migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, dbSchema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// Close closes the underlying DB.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks DB reachability (used by health).
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

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

// --- Services ---

// HasService returns true if a service row exists.
func (s *Store) HasService(ctx context.Context, service string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM services WHERE service=?`, service).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && one == 1, err
}

// SetDeliveryMode sets a user's delivery mode + group chat id.
func (s *Store) SetDeliveryMode(ctx context.Context, tgUserID int64, mode string, groupChatID *int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET delivery_mode=?, group_chat_id=? WHERE tg_user_id=?`,
		mode, groupChatID, tgUserID)
	return err
}

// CreateTokenlessService registers a service with an empty token hash (test/scratch use).
func (s *Store) CreateTokenlessService(ctx context.Context, service, displayName string) error {
	return s.CreateService(ctx, service, displayName, "")
}

// --- service_users + subscriptions ---

// LinkIdentity binds (service,user_ref) to a TG user and ensures a subscription row,
// atomically consuming a link code (UC-2). Returns ErrDuplicateEvent for constraint
// failures and consumes the code ONLY within the same transaction.
func (s *Store) LinkIdentity(ctx context.Context, service, userRef, code string, codeUserID, tgUserID int64) error {
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
	return tx.Commit()
}

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

// --- Delivery routing ---

// RouteTarget is a resolved delivery destination for an event.
type RouteTarget struct {
	UserID   int64
	ChatID   int64
	ThreadID int
	Mode     string // 'group' or 'dm'
}

// DynamicBackend is anything the store calls back into to resolve topics / lazily create
// them. Kept as a func so store does not import tgbot.
type TopicResolver func(ctx context.Context, userID, chatID int64, service string) (int, error)

// ResolveRoutes returns the delivery targets for a routed event: for the (service,user_ref)
// find active service_users, filter by subscription, resolve the topic id in group mode.
func (s *Store) ResolveRoutes(ctx context.Context, e *Event, resolveTopic TopicResolver) ([]RouteTarget, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT su.user_id, u.delivery_mode, u.group_chat_id
		FROM service_users su
		JOIN users u ON u.tg_user_id = su.user_id
		LEFT JOIN subscriptions sub ON sub.user_id = su.user_id AND sub.service = su.service
		WHERE su.service = ? AND su.user_ref = ? AND su.status = 'active'
			AND (sub.muted = 0 OR sub.muted IS NULL)
			AND (sub.event_types IS NULL OR sub.event_types = '' OR
				EXISTS (SELECT 1 FROM json_each(sub.event_types) WHERE json_each.value = ?))`,
		e.Service, e.UserRef, e.Type)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RouteTarget
	for rows.Next() {
		var uid int64
		var mode string
		var gci sql.NullInt64
		if err := rows.Scan(&uid, &mode, &gci); err != nil {
			return nil, err
		}
		rt := RouteTarget{UserID: uid, Mode: mode}
		switch {
		case mode == "group" && gci.Valid:
			tid, err := resolveTopic(ctx, uid, gci.Int64, e.Service)
			if err != nil {
				// Lazy-create failed; still enqueue a DM? No - group lack is a delivery error.
				// We enqueue to group chat with thread 0 so retry can re-create topic.
				rt.ChatID = gci.Int64
				rt.ThreadID = 0
				out = append(out, rt)
				continue
			}
			rt.ChatID = gci.Int64
			rt.ThreadID = tid
		default:
			rt.ChatID = int64(uid) // DM = chat with the user
			rt.ThreadID = 0
		}
		out = append(out, rt)
	}
	return out, rows.Err()
}

// --- group_topics ---

// GetOrCreateTopic returns the topic id for (user,service), creating via resolver if absent.
func (s *Store) GetOrCreateTopic(ctx context.Context, userID, chatID int64, service string, resolver func(chatID int64, service string) (int, error)) (int, error) {
	{
		var tid int
		err := s.db.QueryRowContext(ctx,
			`SELECT message_thread_id FROM group_topics WHERE user_id=? AND service=?`, userID, service).Scan(&tid)
		if err == nil {
			return tid, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	tid, err := resolver(chatID, service)
	if err != nil {
		return 0, err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO group_topics (user_id, service, message_thread_id, created_at)
		VALUES (?,?,?,?) ON CONFLICT(user_id, service) DO NOTHING`,
		userID, service, tid, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return tid, nil
}

// --- deliveries ---

// EnqueueDelivery inserts a pending delivery row.
func (s *Store) EnqueueDelivery(ctx context.Context, d *Delivery) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO deliveries (user_id, event_id, service, type, batch_size, thread_id, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,'pending',?,?)`,
		d.UserID, d.EventID, d.Service, d.Type, d.BatchSize, d.ThreadID, now, now)
	return err
}

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

// PendingDeliveries returns deliveries that are pending+due for delivery (ready).
func (s *Store) PendingDeliveries(ctx context.Context, now time.Time) ([]*Delivery, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, event_id, service, type, batch_size, thread_id, status, attempts, last_err
		 FROM deliveries WHERE status='pending' AND (next_retry_at IS NULL OR next_retry_at <= ?)
		 ORDER BY id LIMIT 500`, now.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Delivery
	for rows.Next() {
		var d Delivery
		var nr sql.NullString
		if err := rows.Scan(&d.ID, &d.UserID, &d.EventID, &d.Service, &d.Type, &d.BatchSize, &d.ThreadID, &d.Status, &d.Attempts, &d.LastErr); err != nil {
			return nil, err
		}
		_ = nr
		// fill chat id from user row
		var gci sql.NullInt64
		var mode string
		er := s.db.QueryRowContext(ctx, `SELECT delivery_mode, group_chat_id FROM users WHERE tg_user_id=?`, d.UserID).Scan(&mode, &gci)
		if er == nil {
			if mode == "group" && gci.Valid {
				d.ChatID = gci.Int64
			} else {
				d.ChatID = d.UserID
			}
		}
		out = append(out, &d)
	}
	return out, rows.Err()
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

// LastDelivered returns the most recent delivered events for a user (for /status).
type DeliverySummary struct {
	ID        int64
	Service   string
	Type      string
	Title     string
	Received  time.Time
	BatchSize int
}

// LatestDeliveriesForUser returns recent deliveries (via events join) for /status.
func (s *Store) LatestDeliveriesForUser(ctx context.Context, userID int64, limit int) ([]DeliverySummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.id, d.service, d.type, e.title, e.received_at, d.batch_size
		FROM deliveries d JOIN events e ON e.event_id = d.event_id
		WHERE d.user_id = ? ORDER BY d.id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeliverySummary
	for rows.Next() {
		var s2 DeliverySummary
		var ra string
		if err := rows.Scan(&s2.ID, &s2.Service, &s2.Type, &s2.Title, &ra, &s2.BatchSize); err != nil {
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339, ra)
		s2.Received = t
		out = append(out, s2)
	}
	return out, rows.Err()
}

// --- codes (connect) ---

// SetTopic stores/persists the topic id for (user,service).
func (s *Store) SetTopic(ctx context.Context, userID int64, service string, threadID int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO group_topics (user_id, service, message_thread_id, created_at)
		VALUES (?,?,?,?) ON CONFLICT(user_id, service) DO UPDATE SET message_thread_id=excluded.message_thread_id`,
		userID, service, threadID, time.Now().UTC().Format(time.RFC3339))
	return err
}

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

// CreateLinkCode stores a link code for a service.
func (s *Store) CreateLinkCode(ctx context.Context, code, service string, userID int64, ttl time.Duration, _ any) error {
	exp := time.Now().UTC().Add(ttl).Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO link_codes (code, service, user_id, expires_at) VALUES (?,?,?,?)`, code, service, userID, exp)
	return err
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

// CountFailed counts failed deliveries for a user.
func (s *Store) CountFailed(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM deliveries WHERE user_id=? AND status='failed'`, userID).Scan(&n)
	return n, err
}

// SaveConnectCode binds a user to a group (alias to CreateConnectCode; kept for clarity).
func (s *Store) SaveConnectCode(ctx context.Context, code string, userID int64, ttl time.Duration) error {
	return s.CreateConnectCode(ctx, code, userID, ttl)
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

// DeliveryAttemptFailed records a failed delivery attempt and schedules next retry.
func (s *Store) DeliveryAttemptFailed(ctx context.Context, id int64, attempts int, next *time.Time, errMsg string) error {
	return s.MarkDeliveryFailed(ctx, id, attempts, next, errMsg)
}

// DeliveryRetry429 records a TG 429 deferral (attempts unchanged).
func (s *Store) DeliveryRetry429(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE deliveries SET last_err='flood limit', updated_at=? WHERE id=?`,
		time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// DeliveryExhausted marks a delivery permanently failed.
func (s *Store) DeliveryExhausted(ctx context.Context, id int64, attempts int, errMsg string) error {
	return s.FailDeliveryPermanently(ctx, id, attempts, errMsg)
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

// EnsureRegistered creates a services row (with an unusable token hash) if absent, so
// catalog services exist in the DB at startup. The admin CLI must rotate/create a real
// token before ingest.
func (s *Store) EnsureRegistered(ctx context.Context, service, displayName string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO services (service, display_name, token_hash, created_at) VALUES (?,?,?,?)
		ON CONFLICT(service) DO NOTHING`,
		service, displayName, "", time.Now().UTC().Format(time.RFC3339))
	return err
}

// QueryRow exposes QueryRowContext for convenience.
func (s *Store) QueryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, q, args...)
}

// --- admin helpers ---

// TokenHash returns the hex sha256 of a raw token (only this is ever persisted/logged).
func TokenHash(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// ServiceList returns all registered services.
type ServiceRec struct {
	Service     string
	DisplayName string
	Enabled     int
}

func (s *Store) ServiceList(ctx context.Context) ([]ServiceRec, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT service, display_name, enabled FROM services ORDER BY service`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServiceRec
	for rows.Next() {
		var r ServiceRec
		if err := rows.Scan(&r.Service, &r.DisplayName, &r.Enabled); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// CreateService registers a service with a raw token, returning the row.
func (s *Store) CreateService(ctx context.Context, service, displayName, rawToken string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO services (service, display_name, token_hash, created_at) VALUES (?,?,?,?)`,
		service, displayName, TokenHash(rawToken), time.Now().UTC().Format(time.RFC3339))
	return err
}

// ServiceEnabled returns whether a service is enabled.
func (s *Store) ServiceEnabled(ctx context.Context, service string) (bool, error) {
	var en int
	err := s.db.QueryRowContext(ctx, `SELECT enabled FROM services WHERE service=?`, service).Scan(&en)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return en == 1 && err == nil, err
}

// VerifyToken returns the service id if the token's hash matches a service's token_hash.
func (s *Store) VerifyToken(ctx context.Context, rawToken string) (string, bool) {
	want := TokenHash(rawToken)
	rows, err := s.db.QueryContext(ctx, `SELECT service FROM services WHERE token_hash=? AND enabled=1`, want)
	if err != nil {
		return "", false
	}
	defer rows.Close()
	if rows.Next() {
		var svc string
		if err := rows.Scan(&svc); err == nil {
			return svc, true
		}
	}
	return "", false
}

// SetEnabled enables/disables a service.
func (s *Store) SetEnabled(ctx context.Context, service string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE services SET enabled=? WHERE service=?`, v, service)
	return err
}

// RotateServiceToken sets a new token_hash, returning nothing (new raw is caller's job to print once).
func (s *Store) RotateServiceToken(ctx context.Context, service, newRaw string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE services SET token_hash=? WHERE service=?`, TokenHash(newRaw), service)
	return err
}

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

// PruneEvents removes events older than the retention window but keeps rows referenced by
// failed deliveries. Returns the number removed.
func (s *Store) PruneEvents(ctx context.Context, retention time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-retention).Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM events
		WHERE received_at < ? AND id NOT IN (
			SELECT MIN(id) FROM deliveries GROUP BY event_id HAVING MAX(CASE WHEN status='failed' THEN 1 ELSE 0 END)=1
		)`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// MarshalMetadata encodes freeform metadata to the events.metadata column.
func MarshalMetadata(v map[string]any) string {
	if v == nil {
		return "{}"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
