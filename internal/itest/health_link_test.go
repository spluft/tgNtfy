package itest

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// /api/health returns 200 from the real handler with a working DB.
func TestHealth200(t *testing.T) {
	h, _ := setupStd(t)
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	h.httpHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health: want 200, got %d", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("ok")) {
		t.Fatalf("health body: %s", rec.Body.String())
	}
}

// postLink POSTs a /v1/link payload with the given token.
func (h *harness) postLink(t *testing.T, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/link", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", token)
	rec := httptest.NewRecorder()
	h.httpHandler.ServeHTTP(rec, req)
	return rec
}

var _ = context.Background

// waitFor polls cond until true or timeout.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting after %v", d)
}
