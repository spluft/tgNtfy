// Service registry rows: tokens (sha256 only), enablement, display names.
package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

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
