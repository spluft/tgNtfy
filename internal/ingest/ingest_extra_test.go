package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spluft/tgNtfy/internal/coalesce"
)

func TestDropUnknownJobProgress(t *testing.T) {
	ts := newTestServer(t)
	tok := ts.seed(t, "goyoutube", "1", 111)
	ev := map[string]any{"v": 1, "event_id": "dp:1", "service": "goyoutube", "user_ref": "1",
		"type": "job_progress", "severity": "info", "title": "t"}
	rec := ts.post(t, "/v1/events", tok, ev)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"queued":0`) {
		t.Fatalf("drop: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEventsServiceFieldMismatch(t *testing.T) {
	ts := newTestServer(t)
	tok := ts.seed(t, "goyoutube", "1", 111)
	ev := mkEvent("mm:1")
	ev["service"] = "gomail"
	rec := ts.post(t, "/v1/events", tok, ev)
	if rec.Code != 400 {
		t.Fatalf("mismatch: want 400, got %d", rec.Code)
	}
}

func TestLinkErrorFlows(t *testing.T) {
	ts := newTestServer(t)
	tok := ts.seed(t, "goyoutube", "1", 111)
	ts.st.EnsureRegistered(context.Background(), "goyoutube", "goYouTube")

	req := httptest.NewRequest(http.MethodPost, "/v1/link", strings.NewReader(`{"service":"goyoutube","user_ref":"17","code":"nope"}`))
	req.Header.Set("X-Service-Token", tok)
	rec := httptest.NewRecorder()
	ts.h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "code_invalid") {
		t.Fatalf("bogus code: code=%d body=%s", rec.Code, rec.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodPost, "/v1/link", strings.NewReader(`{"service":"goyoutube","user_ref":"17","code":"x","display_name":"`+strings.Repeat("x", 300)+`"}`))
	req2.Header.Set("X-Service-Token", tok)
	rec2 := httptest.NewRecorder()
	ts.h.Routes().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("long display_name: want 400, got %d", rec2.Code)
	}
}

func TestRenderHelpers(t *testing.T) {
	single := RenderMessage("success", "goYouTube", "job_completed", "done", "some text", "https://x")
	if !strings.Contains(single, "goYouTube") || !strings.Contains(single, "some text") {
		t.Fatalf("single: %q", single)
	}
	long := strings.Repeat("y", 600)
	clipped := RenderMessage("info", "S", "T", "tt", long, "")
	if len(clipped) > 3500 {
		t.Fatalf("clipped length %d", len(clipped))
	}

	items := []*coalesce.Item{
		{UserID: 1, URL: "https://shared", Title: "a"},
		{UserID: 1, URL: "https://shared", Title: "b"},
	}
	bat := RenderBatch("warn", "S", "T", items)
	if !strings.Contains(bat, "https://shared") {
		t.Fatalf("batch missing shared url: %q", bat)
	}
	items[1].URL = "https://other"
	bat2 := RenderBatch("warn", "S", "T", items)
	if strings.Contains(bat2, "https://shared") {
		t.Fatalf("batch must drop shared url when divergent: %q", bat2)
	}
}
