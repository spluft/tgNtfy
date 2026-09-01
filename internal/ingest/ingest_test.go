package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spluft/tgNtfy/internal/catalog"
	"github.com/spluft/tgNtfy/internal/store"
)

// testSrv builds a fully-wired ingest handler with an in-memory/temp sqlite db and a
// no-op coalescer (so tests exercise routing + idempotency deterministically).
type testSrv struct {
	st *store.Store
	h  *Handler
}

func newTestServer(t *testing.T) *testSrv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cat, err := catalog.Load(filepath.Join("..", "..", "config", "events.yaml"))
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	lk := catalog.NewLookup(cat)
	h := NewHandler(st, lk, nil)
	return &testSrv{st: st, h: h}
}

// seed creates a service, a user, links (service,user_ref), and (optionally) a
// group+topic, then returns the raw token.
func (ts *testSrv) seed(t *testing.T, svc, userRef string, tgUser int64) string {
	t.Helper()
	ctx := context.Background()
	tok := "seedtoken-" + svc
	if err := ts.st.CreateService(ctx, svc, svc, tok); err != nil {
		t.Fatalf("create service: %v", err)
	}
	if err := ts.st.UpsertUser(ctx, tgUser, "user"+strings.TrimLeft(tok, "seedtoken-"), "U"); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	if err := ts.st.EnsureServiceUser(ctx, svc, userRef, tgUser); err != nil {
		t.Fatalf("ensure service_user: %v", err)
	}
	return tok
}

func (ts *testSrv) post(t *testing.T, path string, tok string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r ioReader
	var err error
	if s, ok := body.(string); ok {
		r = strings.NewReader(s)
	} else {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(http.MethodPost, path, r)
	req.Header.Set("X-Service-Token", tok)
	rec := httptest.NewRecorder()
	ts.h.Routes().ServeHTTP(rec, req)
	_ = err
	return rec
}

func mkEvent(id string) map[string]any {
	return map[string]any{
		"v": 1, "event_id": id, "service": "goyoutube", "user_ref": "1",
		"type": "job_completed", "severity": "success",
		"title": "Video downloaded", "text": "", "url": "",
	}
}

func TestEventsUnauthorized(t *testing.T) {
	ts := newTestServer(t)
	ts.seed(t, "goyoutube", "1", 111)
	rec := ts.post(t, "/v1/events", "wrongtoken", mkEvent("go:1"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unauthorized") {
		t.Fatalf("body: %s", rec.Body.String())
	}
}

func TestEventsSchemaErrors(t *testing.T) {
	ts := newTestServer(t)
	tok := ts.seed(t, "goyoutube", "1", 111)
	for name, body := range map[string]map[string]any{
		"bad_v":      {"v": 2, "event_id": "x", "service": "goyoutube", "user_ref": "1", "type": "job_completed", "severity": "info", "title": "t"},
		"bad_sev":    {"v": 1, "event_id": "x", "service": "goyoutube", "user_ref": "1", "type": "job_completed", "severity": "boom", "title": "t"},
		"no_title":   {"v": 1, "event_id": "x", "service": "goyoutube", "user_ref": "1", "type": "job_completed", "severity": "info"},
		"no_userref": {"v": 1, "event_id": "x", "service": "goyoutube", "type": "job_completed", "severity": "info", "title": "t"},
	} {
		body := body
		rec := ts.post(t, "/v1/events", tok, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d (%s)", name, rec.Code, rec.Body.String())
		}
	}
}

func TestEventsBodyTooLarge(t *testing.T) {
	ts := newTestServer(t)
	tok := ts.seed(t, "goyoutube", "1", 111)
	big := "{" + strings.Repeat("x", 8193) + "}"
	rec := ts.post(t, "/v1/events", tok, big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d", rec.Code)
	}
}

func TestEventsTooManyRequests(t *testing.T) {
	ts := newTestServer(t)
	tok := ts.seed(t, "goyoutube", "1", 111)
	for i := 0; i < 30; i++ {
		ev := mkEvent("go:burst:" + string(rune('a'+i)))
		if rec := ts.post(t, "/v1/events", tok, ev); rec.Code != http.StatusOK {
			t.Fatalf("event %d: want 200, got %d", i, rec.Code)
		}
	}
	// 31st within the same second -> 429
	rec := ts.post(t, "/v1/events", tok, mkEvent("go:burst:zz"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("31st: want 429, got %d", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("expected Retry-After header on 429")
	}
}

func TestIdempotencyDuplicate409(t *testing.T) {
	ts := newTestServer(t)
	tok := ts.seed(t, "goyoutube", "1", 111)
	ev := mkEvent("dup:1")
	r1 := ts.post(t, "/v1/events", tok, ev)
	if r1.Code != http.StatusOK {
		t.Fatalf("first: want 200, got %d", r1.Code)
	}
	r2 := ts.post(t, "/v1/events", tok, ev)
	if r2.Code != http.StatusConflict {
		t.Fatalf("duplicate: want 409, got %d", r2.Code)
	}
	if !strings.Contains(r2.Body.String(), "duplicate") {
		t.Fatalf("body: %s", r2.Body.String())
	}
}

func TestLinkFlow(t *testing.T) {
	ts := newTestServer(t)
	tok := ts.seed(t, "goyoutube", "99", 777)
	// create a link code owned by tg user 777 for service goyoutube
	if err := ts.st.EnsureRegistered(context.Background(), "goyoutube", "goYouTube"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := ts.h.store.CreateLinkCode(context.Background(), "123456", "goyoutube", 777, 10*time.Minute, nil); err != nil {
		t.Fatalf("create link code: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/link", strings.NewReader(`{"service":"goyoutube","user_ref":"17","code":"123456"}`))
	req.Header.Set("X-Service-Token", tok)
	rec := httptest.NewRecorder()
	ts.h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("link: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	// Code single-use -> second is rejected
	req2 := httptest.NewRequest(http.MethodPost, "/v1/link", strings.NewReader(`{"service":"goyoutube","user_ref":"18","code":"123456"}`))
	req2.Header.Set("X-Service-Token", tok)
	rec2 := httptest.NewRecorder()
	ts.h.Routes().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("reuse: want 400, got %d (%s)", rec2.Code, rec2.Body.String())
	}
}

// ioReader chosen to satisfy both strings.Reader and *bytes.Reader across the helper.
type ioReader interface {
	Read([]byte) (int, error)
}
