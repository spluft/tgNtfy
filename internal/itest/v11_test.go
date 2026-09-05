// v1.1 integration tests (binding SPEC §11): service-agnostic lazy topic creation at
// registration, re-link idempotency, dm-mode deferral + first-event fallback, and the
// empty/absent-catalog contract.
package itest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spluft/tgNtfy/internal/catalog"
)

// FOR-3 (T-LINK-1, V11-2) — POSITIVE registration-topic test (closes v1 acceptance
// FAIL #2): a user already in group mode links S via code with "  Go YouTube  x!".
// Exactly ONE createForumTopic with the SANITIZED name is recorded, topic_created=true, the
// message_thread_id is persisted in group_topics, and a following event is delivered with it.
func TestFOR3LinkCreatesTopicAtRegistration(t *testing.T) {
	h, mock := setupStd(t)
	tok := h.seedService(t, "goyoutube")
	h.seedUser(t, "goyoutube", "17", 111)
	h.setGroup(t, 111, 9001)

	if err := h.st.CreateLinkCode(context.Background(), "222111", "goyoutube", 111, time.Minute, nil); err != nil {
		t.Fatalf("create link code: %v", err)
	}
	rec := h.postLink(t, tok, `{"service":"goyoutube","user_ref":"17","code":"222111","display_name":"  Go YouTube  x!"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("link: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status       string `json:"status"`
		TopicCreated bool   `json:"topic_created"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode link response: %v (%s)", err, rec.Body.String())
	}
	if resp.Status != "linked" || !resp.TopicCreated {
		t.Fatalf("link response = %+v, want status=linked topic_created=true", resp)
	}

	tps := mock.createTopics()
	if len(tps) != 1 {
		t.Fatalf("registration must call createForumTopic exactly once, got %d", len(tps))
	}
	if name := stringify(tps[0].Form["name"]); name != "Go YouTube x!" {
		t.Fatalf("forum topic name = %q, want %q (sanitized)", name, "Go YouTube x!")
	}

	tid, _, err := h.st.GetTopicThread(context.Background(), 111, "goyoutube")
	if err != nil {
		t.Fatalf("group_topics row missing after link: %v", err)
	}
	if tid == 0 {
		t.Fatal("group_topics message_thread_id = 0, want a real thread id")
	}

	// A following event for (user,service) is delivered with that message_thread_id.
	if rec := h.postEvent(t, tok, mkEv("goyoutube", "job_completed", "for3-ev", "17")); rec.Code != http.StatusOK {
		t.Fatalf("event: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	waitFor(t, 4*time.Second, func() bool { return len(mock.sendMessages()) >= 1 })
	sends := mock.sendMessages()
	if len(sends) == 0 {
		t.Fatal("no sendMessage for the linked event")
	}
	if got := stringify(sends[len(sends)-1].Form["chat_id"]); got != "9001" {
		t.Fatalf("event chat_id = %q, want 9001", got)
	}
	if got := stringify(sends[len(sends)-1].Form["message_thread_id"]); got != itoa(tid) {
		t.Fatalf("event message_thread_id = %q, want %q (persisted id %d)", got, itoa(tid), tid)
	}
	// V-5 fix: the event path reuses the row — no second createForumTopic.
	if now := len(mock.createTopics()); now != 1 {
		t.Fatalf("event path must reuse the row, createForumTopic now=%d want 1", now)
	}
}

// FOR-4 + T-LINK-3 + T-FALL-1 (V-3/V-5): a user in dm mode links S with a display name
// -> 200, topic_created=false, ZERO createForumTopic (deferred — locks in "a link that creates
// no topic is CORRECT"); services.display_name is updated. After the user binds a group (no row),
// the FIRST event lazily creates exactly ONE topic with the STORE display name, persists it, and
// delivers with that thread_id. A second event reuses the row (no new topic).
func TestFOR4DMDeferralThenFirstEventFallback(t *testing.T) {
	h, mock := setupStd(t)
	tok := h.seedServiceNamed(t, "goyoutube", "AdminDefault")
	h.seedUser(t, "goyoutube", "17", 111) // stays dm

	// T-LINK-3: dm-mode link defers the topic.
	if err := h.st.CreateLinkCode(context.Background(), "333444", "goyoutube", 111, time.Minute, nil); err != nil {
		t.Fatalf("create link code: %v", err)
	}
	rec := h.postLink(t, tok, `{"service":"goyoutube","user_ref":"17","code":"333444","display_name":"  MyService Label  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("dm link: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status       string `json:"status"`
		TopicCreated bool   `json:"topic_created"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.TopicCreated {
		t.Fatal("dm-mode link must not create a topic (topic_created=false)")
	}
	if got := len(mock.createTopics()); got != 0 {
		t.Fatalf("dm link must create ZERO topics, got %d", got)
	}
	if dn, _ := h.st.DisplayName(context.Background(), "goyoutube"); dn != "MyService Label" {
		t.Fatalf("services.display_name = %q, want %q", dn, "MyService Label")
	}

	// T-FALL-1 / FOR-4: bind a group (no topic row yet) -> first event creates the topic.
	h.setGroup(t, 111, 9010)
	if rec := h.postEvent(t, tok, mkEv("goyoutube", "job_completed", "for4-ev1", "17")); rec.Code != http.StatusOK {
		t.Fatalf("event1: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	waitFor(t, 4*time.Second, func() bool { return len(mock.sendMessages()) >= 1 })
	tps := mock.createTopics()
	if len(tps) != 1 {
		t.Fatalf("first event must create exactly ONE topic, got %d", len(tps))
	}
	if name := stringify(tps[0].Form["name"]); name != "MyService Label" {
		t.Fatalf("fallback topic name = %q, want %q (store display name, NOT catalog)", name, "MyService Label")
	}
	tid, _, err := h.st.GetTopicThread(context.Background(), 111, "goyoutube")
	if err != nil {
		t.Fatalf("fallback topic not persisted: %v", err)
	}
	sends := mock.sendMessages()
	if len(sends) == 0 {
		t.Fatal("no sendMessage after first event")
	}
	if got := stringify(sends[len(sends)-1].Form["message_thread_id"]); got != itoa(tid) {
		t.Fatalf("first-event message_thread_id = %q, want %q", got, itoa(tid))
	}

	// V-5 fix: a second event reuses the row -> no additional createForumTopic.
	if rec := h.postEvent(t, tok, mkEv("goyoutube", "job_completed", "for4-ev2", "17")); rec.Code != http.StatusOK {
		t.Fatalf("event2: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	waitFor(t, 4*time.Second, func() bool { return len(mock.sendMessages()) >= 2 })
	if now := len(mock.createTopics()); now != 1 {
		t.Fatalf("second event must NOT create another topic, createForumTopic now=%d want 1", now)
	}
}

// IDL-1 + T-LINK-2 (V-4, V11-3): re-linking the same (user,service) is idempotent — the
// existing group_topics row is reused, NO duplicate topic, and a fresh display_name updates
// services.display_name. A fresh code for the same user_ref bound to a DIFFERENT TG user -> 409.
func TestIDL1RelinkIdempotentNoDuplicate(t *testing.T) {
	h, mock := setupStd(t)
	tok := h.seedService(t, "goyoutube")
	h.seedUser(t, "goyoutube", "17", 111)
	h.setGroup(t, 111, 9001)

	// first link
	if err := h.st.CreateLinkCode(context.Background(), "500001", "goyoutube", 111, time.Minute, nil); err != nil {
		t.Fatalf("code1: %v", err)
	}
	if rec := h.postLink(t, tok, `{"service":"goyoutube","user_ref":"17","code":"500001","display_name":"GoYouTube"}`); rec.Code != http.StatusOK {
		t.Fatalf("link1: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if got := len(mock.createTopics()); got != 1 {
		t.Fatalf("link1 must create exactly one topic, got %d", got)
	}
	tid1, _, err := h.st.GetTopicThread(context.Background(), 111, "goyoutube")
	if err != nil {
		t.Fatalf("topic1 row: %v", err)
	}

	// re-link: fresh code, same user+service, different display_name -> 200 (not 409), row reused.
	if err := h.st.CreateLinkCode(context.Background(), "500002", "goyoutube", 111, time.Minute, nil); err != nil {
		t.Fatalf("code2: %v", err)
	}
	rec2 := h.postLink(t, tok, `{"service":"goyoutube","user_ref":"17","code":"500002","display_name":"goYouTube Renamed"}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("re-link: want 200 (idempotent refresh), got %d (%s)", rec2.Code, rec2.Body.String())
	}
	if got := len(mock.createTopics()); got != 1 {
		t.Fatalf("re-link must NOT create a duplicate topic, createForumTopic now=%d want 1", got)
	}
	if tid2, _, err := h.st.GetTopicThread(context.Background(), 111, "goyoutube"); err != nil || tid2 != tid1 {
		t.Fatalf("re-link must reuse the row: tid1=%d tid2=%d err=%v", tid1, tid2, err)
	}
	if dn, _ := h.st.DisplayName(context.Background(), "goyoutube"); dn != "goYouTube Renamed" {
		t.Fatalf("re-link must update services.display_name, got %q", dn)
	}

	// variant: same user_ref bound to a DIFFERENT TG user -> 409 already_linked.
	if err := h.st.UpsertUser(context.Background(), 200, "u200", "U2"); err != nil {
		t.Fatalf("upsert user 200: %v", err)
	}
	if err := h.st.CreateLinkCode(context.Background(), "500003", "goyoutube", 200, time.Minute, nil); err != nil {
		t.Fatalf("code3: %v", err)
	}
	if rec3 := h.postLink(t, tok, `{"service":"goyoutube","user_ref":"17","code":"500003"}`); rec3.Code != http.StatusConflict {
		t.Fatalf("cross-user re-link: want 409, got %d (%s)", rec3.Code, rec3.Body.String())
	}
}

// CAT-1 (T-CAT-1, V11-4): the gate operates end-to-end with an EMPTY catalog — no catalog
// entry is required for boot, ingest, routing, topic creation, rendering, or delivery. Link works
// (store-driven), the topic is created with the STORE display name, and the rendered message carries it.
func TestCAT1EmptyCatalogOperations(t *testing.T) {
	mock, err := newMockBot(testBotToken)
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	t.Cleanup(mock.Close)
	h := newHarness(t, mock, &harnessOpts{cat: &catalog.Catalog{Version: 1, Services: map[string]catalog.Service{}}})
	tok := h.seedServiceNamed(t, "goyoutube", "Alpha Service")
	h.seedUser(t, "goyoutube", "17", 111)
	h.setGroup(t, 111, 9001)

	// /v1/link works with an empty catalog (store-driven).
	if err := h.st.CreateLinkCode(context.Background(), "600777", "goyoutube", 111, time.Minute, nil); err != nil {
		t.Fatalf("link code: %v", err)
	}
	if rec := h.postLink(t, tok, `{"service":"goyoutube","user_ref":"17","code":"600777"}`); rec.Code != http.StatusOK {
		t.Fatalf("link under empty catalog: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	// Ingest + route + lazy topic (store name) + deliver all work without a catalog entry.
	if rec := h.postEvent(t, tok, mkEv("goyoutube", "job_completed", "cat1-ev", "17")); rec.Code != http.StatusOK {
		t.Fatalf("event under empty catalog: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	waitFor(t, 4*time.Second, func() bool { return len(mock.sendMessages()) >= 1 })
	tps := mock.createTopics()
	if len(tps) != 1 {
		t.Fatalf("empty-catalog topic should be created once; got %d", len(tps))
	}
	if name := stringify(tps[0].Form["name"]); name != "Alpha Service" {
		t.Fatalf("empty-catalog topic name = %q, want %q (from the STORE)", name, "Alpha Service")
	}
	sends := mock.sendMessages()
	if len(sends) == 0 {
		t.Fatal("no delivery under empty catalog")
	}
	if text := stringify(sends[len(sends)-1].Form["text"]); !strings.Contains(text, "Alpha Service") {
		t.Fatalf("rendered message must contain the STORE display name, got: %q", text)
	}
}

// CAT-2 (T-CAT-1 absent-file boot, UC-V4/V11-4): catalog.Load on a missing path returns an
// os.ErrNotExist error; a main-style construction with an empty catalog + warn still yields a
// working lookup (no hard-fail) — CAT-1 then proves the full pipeline end-to-end on it.
func TestCAT2AbsentCatalogLoadError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist-events.yaml")
	if _, err := catalog.Load(missing); err == nil {
		t.Fatal("catalog.Load on a missing path must return an error")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
	lk := catalog.NewLookup(&catalog.Catalog{Version: 1, Services: map[string]catalog.Service{}})
	if lk.Get() == nil {
		t.Fatal("empty Lookup must yield a readable snapshot")
	}
}
