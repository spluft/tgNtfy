package store

import "context"

// CreateTokenlessService registers a service with an empty token hash
// (test/scratch use). Test-only helper: lives in export_test.go so production
// files expose only what the server calls (SPEC t_2d992300 §6).
func (s *Store) CreateTokenlessService(ctx context.Context, service, displayName string) error {
	return s.CreateService(ctx, service, displayName, "")
}
