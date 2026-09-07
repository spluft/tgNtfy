package healthz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthOK(t *testing.T) {
	h := &Health{PingFunc: func(ctx context.Context) error { return nil }}
	rec := httptest.NewRecorder()
	h.HandleHealth("")(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
}

func TestHealthDBDown(t *testing.T) {
	h := &Health{PingFunc: func(ctx context.Context) error { return errors.New("down") }}
	rec := httptest.NewRecorder()
	h.HandleHealth("")(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
}

func TestHealthAdminTokenAuth(t *testing.T) {
	h := &Health{PingFunc: func(ctx context.Context) error { return nil }}
	// token set but missing -> 401
	rec := httptest.NewRecorder()
	h.HandleHealth("sekret")(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: want 401, got %d", rec.Code)
	}
	// matching -> 200
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Header.Set("X-Admin-Token", "sekret")
	rec2 := httptest.NewRecorder()
	h.HandleHealth("sekret")(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("matching token: want 200, got %d", rec2.Code)
	}
	// wrong -> 401
	req3 := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req3.Header.Set("X-Admin-Token", "wrong")
	rec3 := httptest.NewRecorder()
	h.HandleHealth("sekret")(rec3, req3)
	if rec3.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: want 401, got %d", rec3.Code)
	}
}
