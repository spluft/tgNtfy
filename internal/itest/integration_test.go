// Integration tests for the tgNtfy gate behavioral ACs, driven through the real HTTP
// ingest path against a mock Telegram Bot API (test-only package; the qa commit is
// allowed to contain only _test.go files).
//
//	ISO-1  isolation:   user-A's event never reaches user-B's group/topic
//	IDP-1  idempotency: duplicate event_id within 24h -> 409, one delivery only
//	COA-1  coalesce:    production config (5s window, cap 20) collapses 60 events to <=3
//	FOR-1  forum path:   event->forum topic delivery uses message_thread_id
//	FOR-2  setup gates:  sender-not-admin / bot-no-manage-topics / non-forum keep 'dm';
//	                       successful /connect binds group + clears stale topics, no topics created (V-9)
//	RATE-1 per-service rate limit: 31st in a 1s burst -> 429; services independent
//	AUTH   wrong token 401; oversize 413; malformed json 400
//	LINK   /v1/link lifecycle: bind, single-use, unknown/used code -> 400
package itest

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
)

const testBotToken = "123456789:TESTTOKENTESTTOKENTESTTOKEN"

func setupStd(t *testing.T) (*harness, *mockBot) {
	t.Helper()
	mock, err := newMockBot(testBotToken)
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	t.Cleanup(mock.Close)
	h := newHarness(t, mock, nil)
	return h, mock
}

// mkEv builds a valid envelope for any service/type.
func mkEv(svc, typ, id, userRef string) map[string]any {
	return map[string]any{
		"v": 1, "event_id": id, "service": svc, "user_ref": userRef,
		"type": typ, "severity": "success", "title": "Job done", "text": "", "url": "",
	}
}

// ISO-1: user + group + topic for A(111/9001) and B(222/9002). An event for A must
// produce exactly one sendMessage to chat 9001 (never 9002), carrying a message_thread_id.
func TestISO1Isolation(t *testing.T) {
	h, mock := setupStd(t)
	tok := h.seedService(t, "goyoutube")
	h.seedUser(t, "goyoutube", "1", 111) // user A
	h.seedUser(t, "goyoutube", "2", 222) // user B
	h.setGroup(t, 111, 9001)
	h.setGroup(t, 222, 9002)
	_ = h.st.SetTopic(context.Background(), 111, "goyoutube", 77)
	_ = h.st.SetTopic(context.Background(), 222, "goyoutube", 88)

	if rec := h.postEvent(t, tok, mkEv("goyoutube", "job_completed", "iso-a-1", "1")); rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	waitFor(t, 4*time.Second, func() bool { return len(mock.sendMessages()) >= 1 })

	sends := mock.sendMessages()
	if len(sends) != 1 {
		t.Fatalf("expected exactly 1 sendMessage, got %d", len(sends))
	}
	if sends[0].Form["chat_id"] != "9001" {
		t.Errorf("ISO-1 chat_id=%v want 9001 (user A only)", sends[0].Form["chat_id"])
	}
	if tid := stringify(sends[0].Form["message_thread_id"]); tid == "" {
		t.Errorf("ISO-1: sendMessage missing message_thread_id")
	}
	for _, s := range sends {
		if s.Form["chat_id"] == "9002" {
			t.Errorf("ISO-1: user B's group received a message: %v", s.Form)
		}
	}
}

// IDP-1: same event_id twice -> 409; no second delivery.
func TestIDP1Idempotent409(t *testing.T) {
	h, mock := setupStd(t)
	tok := h.seedService(t, "goyoutube")
	h.seedUser(t, "goyoutube", "1", 111)

	if rec := h.postEvent(t, tok, mkEv("goyoutube", "job_completed", "idp-dup", "1")); rec.Code != http.StatusOK {
		t.Fatalf("first: want 200, got %d", rec.Code)
	}
	waitFor(t, 4*time.Second, func() bool { return len(mock.sendMessages()) >= 1 })
	before := len(mock.sendMessages())

	rec2 := h.postEvent(t, tok, mkEv("goyoutube", "job_completed", "idp-dup", "1"))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("duplicate: want 409, got %d (%s)", rec2.Code, rec2.Body.String())
	}
	time.Sleep(150 * time.Millisecond)
	if got := len(mock.sendMessages()); got != before {
		t.Fatalf("duplicate event must not produce another delivery: before=%d after=%d", before, got)
	}
}

// COA-1 (rewritten, v1.1 SPEC §7/V-8): 60 same-type events for one (user,service) under
// the PRODUCTION config (5s coalesce window, batch cap 20) produce <= 3 messages;
// sum(batch_size) == 60; every batch <= 20. v1 acceptance measured exactly 3
// (ceil(60/20)) — the batch cap makes the old "<=2" AC unsatisfiable (owner-approved).
func TestCOA1CoalesceBurst(t *testing.T) {
	mock, err := newMockBot(testBotToken)
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	t.Cleanup(mock.Close)
	h := newHarness(t, mock, &harnessOpts{coalesceWindow: 5 * time.Second, coalesceCap: 20})
	tok := h.seedService(t, "goyoutube")
	h.seedUser(t, "goyoutube", "1", 111)
	h.setGroup(t, 111, 9001)
	_ = h.st.SetTopic(context.Background(), 111, "goyoutube", 5)

	const n = 60
	accepted := 0
	for i := 0; i < n; i++ {
		rec := h.postEvent(t, tok, mkEv("goyoutube", "job_completed", "coa-v11-"+itoa(i), "1"))
		if rec.Code == http.StatusOK {
			accepted++
		}
		time.Sleep(40 * time.Millisecond) // 25/s -> under the 30/s burst cap; cap-20 flushes early
	}
	if accepted != n {
		t.Fatalf("all %d must be accepted under rate limit (got %d)", n, accepted)
	}
	waitFor(t, 8*time.Second, func() bool { return len(mock.sendMessages()) >= 3 })
	time.Sleep(300 * time.Millisecond)
	msgs := mock.sendMessages()
	if len(msgs) > 3 {
		t.Fatalf("COA-1: production config must produce <=3 messages for 60 events, got %d", len(msgs))
	}
	if len(msgs) < 1 {
		t.Fatalf("COA-1: expected at least one delivery, got none")
	}
	sum, maxB, err := h.st.DeliveryBatchStats(context.Background(), 111, "goyoutube")
	if err != nil {
		t.Fatalf("query deliveries: %v", err)
	}
	if sum != n {
		t.Fatalf("COA-1: sum(batch_size)=%d, want %d", sum, n)
	}
	if maxB > 20 {
		t.Fatalf("COA-1: a coalesce batch exceeded the cap 20: %d", maxB)
	}
}

// FOR-1: user in group mode; an event for a linked service resolves a forum topic and sendMessage
// carries message_thread_id.
func TestFOR1ForumDelivery(t *testing.T) {
	h, mock := setupStd(t)
	tok := h.seedService(t, "goyoutube")
	h.seedUser(t, "goyoutube", "1", 111)
	h.setGroup(t, 111, 9001)

	if rec := h.postEvent(t, tok, mkEv("goyoutube", "job_completed", "for-1", "1")); rec.Code != http.StatusOK {
		t.Fatalf("event: want 200, got %d", rec.Code)
	}
	waitFor(t, 4*time.Second, func() bool { return len(mock.sendMessages()) >= 1 })
	if len(mock.sendMessages()) == 0 {
		t.Fatalf("expected at least one sendMessage")
	}
	if tid := stringify(mock.sendMessages()[0].Form["message_thread_id"]); tid == "" {
		t.Fatalf("sendMessage must carry message_thread_id in forum mode")
	}
}

// FOR-2: S2 setup gates (verifySetup via "setup:done:<uid>" callback). Each failure sends the
// matching error and leaves the user in 'dm' with no topics created. A successful /connect
// (V-9) binds the group, DELETES stale group_topics, and creates ZERO topics.
func TestFOR2SetupGatesKeepDM(t *testing.T) {
	mock, err := newMockBot(testBotToken)
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	t.Cleanup(mock.Close)
	h := newHarness(t, mock, nil)
	h.seedService(t, "goyoutube")
	h.seedUser(t, "goyoutube", "1", 111)

	// case 1: group isn't a forum -> stays dm.
	mock.isForum = false
	h.menuCallback(111, 9001, "setup:done:111")
	if u, _ := h.st.GetUser(context.Background(), 111); u != nil && u.DeliveryMode != "dm" {
		t.Fatalf("non-forum setup must keep dm, got %q", u.DeliveryMode)
	}
	if cups := mock.createTopics(); len(cups) != 0 {
		t.Fatalf("no topics on non-forum, got %d", len(cups))
	}

	// case 2: forum but bot lacks manage-topics -> stays dm, no topics.
	mock.isForum = true
	mock.botAdmTopics = false
	mock.senderAdmin = true
	h.menuCallback(111, 9001, "setup:done:111")
	if u, _ := h.st.GetUser(context.Background(), 111); u != nil && u.DeliveryMode != "dm" {
		t.Fatalf("bot-no-manage-topics must keep dm, got %q", u.DeliveryMode)
	}
	if cups := mock.createTopics(); len(cups) != 0 {
		t.Fatalf("no topics on missing right, got %d", len(cups))
	}

	// case 3: forum+rights OK but sender not admin -> stays dm, no topics.
	mock.botAdmTopics = true
	mock.senderAdmin = false
	h.menuCallback(111, 9001, "setup:done:111")
	if u, _ := h.st.GetUser(context.Background(), 111); u != nil && u.DeliveryMode != "dm" {
		t.Fatalf("sender-not-admin must keep dm, got %q", u.DeliveryMode)
	}
	if cups := mock.createTopics(); len(cups) != 0 {
		t.Fatalf("no topics on sender-not-admin, got %d", len(cups))
	}

	// V-9: happy /connect binds the group, clears any stale group_topics row for the user,
	// and creates ZERO forum topics (topics appear lazily per linked service on link/first event).
	mock.senderAdmin = true
	mock.botAdmTopics = true
	const connectCode = "605060"
	if err := h.st.SetTopic(context.Background(), 111, "goyoutube", 42); err != nil {
		t.Fatalf("seed stale topic: %v", err)
	}
	if err := h.st.CreateConnectCode(context.Background(), connectCode, 111, time.Minute); err != nil {
		t.Fatalf("create connect code: %v", err)
	}
	before := len(mock.createTopics())
	h.menuUpdate(111, "/connect "+connectCode, models.Chat{ID: 9001, Type: models.ChatTypeSupergroup})
	u, _ := h.st.GetUser(context.Background(), 111)
	if u == nil || u.DeliveryMode != "group" || u.GroupChatID == nil || *u.GroupChatID != 9001 {
		t.Fatalf("V-9: /connect must bind group mode + chat 9001, got %+v", u)
	}
	if staleTid, found, _ := h.st.GetTopicThread(context.Background(), 111, "goyoutube"); found {
		t.Fatalf("V-9: /connect must clear stale group_topic row, found thread %d", staleTid)
	}
	if got := len(mock.createTopics()); got != before {
		t.Fatalf("V-9: /connect must create ZERO forum topics, got %d (before %d)", got, before)
	}
}

// RATE-1: per-service rate limit keeps burst <=30/s then 429; other services stay independent.
func TestRATE1SustainedAndBurst(t *testing.T) {
	h, _ := setupStd(t)
	tokYT := h.seedService(t, "goyoutube")
	tokMail := h.seedService(t, "gomail")
	h.seedUser(t, "goyoutube", "1", 111)
	h.seedUser(t, "gomail", "1", 111)

	burstOK, burst429 := 0, 0
	for i := 0; i < 32; i++ {
		rec := h.postEvent(t, tokYT, mkEv("goyoutube", "job_completed", "rate-"+itoa(i), "1"))
		switch rec.Code {
		case http.StatusOK:
			burstOK++
		case http.StatusTooManyRequests:
			burst429++
		}
	}
	if burstOK < 30 || burst429 < 1 {
		t.Fatalf("burst: want >=30 OK and >=1 429, got ok=%d n429=%d", burstOK, burst429)
	}
	if rec := h.postEvent(t, tokMail, mkEv("gomail", "new-mail", "mail-1", "1")); rec.Code != http.StatusOK {
		t.Fatalf("gomail must stay independent, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// AUTH/size/schema errors through the real HTTP path.
func TestAUTHAndSizeAndSchemaErrors(t *testing.T) {
	h, _ := setupStd(t)
	tok := h.seedService(t, "goyoutube")

	if rec := h.postEvent(t, "badtoken", mkEv("goyoutube", "job_completed", "auth-1", "1")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: want 401, got %d", rec.Code)
	}
	big := `{"v":1,"title":"` + strings.Repeat("x", 9000) + `"}`
	if rec := h.postEvent(t, tok, big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: want 413, got %d", rec.Code)
	}
	if rec := h.postEvent(t, tok, `{not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed: want 400, got %d", rec.Code)
	}
}

// LINK: valid single-use code binds; used and unknown codes -> 400.
func TestLinkLifecycle(t *testing.T) {
	h, _ := setupStd(t)
	tok := h.seedService(t, "goyoutube")
	h.seedUser(t, "goyoutube", "17", 113)

	if err := h.st.CreateLinkCode(context.Background(), "100200", "goyoutube", 113, time.Minute, nil); err != nil {
		t.Fatalf("create link code: %v", err)
	}
	if rec := h.postLink(t, tok, `{"service":"goyoutube","user_ref":"17","code":"100200"}`); rec.Code != http.StatusOK {
		t.Fatalf("valid code: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := h.postLink(t, tok, `{"service":"goyoutube","user_ref":"18","code":"100200"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("used code: want 400, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := h.postLink(t, tok, `{"service":"goyoutube","user_ref":"19","code":"000000"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown code: want 400, got %d", rec.Code)
	}
}

func stringify(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
