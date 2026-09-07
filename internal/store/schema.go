// Package store is the single owner of SQLite persistence. It uses
// modernc.org/sqlite (pure Go, no cgo). All events are persisted here; a single
// writer path (via the shared *sql.DB which serializes writes) plus pragmatic
// readers. PRAGMAs follow the SPEC: WAL, busy_timeout, synchronous=NORMAL,
// foreign_keys=ON.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	_ "modernc.org/sqlite"
	"time"
)

// Well-known sentinel errors.
var (
	ErrDuplicateEvent = errors.New("duplicate event_id within retention window")
	ErrCodeInvalid    = errors.New("code not found or expired")
	ErrCodeUsed       = errors.New("code already used")
	ErrCodeMismatch   = errors.New("code bound to a different service")
	ErrAlreadyLinked  = errors.New("service user_ref already linked to another user")
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
