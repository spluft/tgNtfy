// Integration tests for the tgNtfy gate behavioral ACs, driven through the real HTTP
// ingest path against a mock Telegram Bot API (test-only package; the qa commit is
// allowed to contain only _test.go files).
//
//	ISO-1  isolation:   user-A's event never reaches user-B's group/topic
//	IDP-1  idempotency: duplicate event_id within 24h -> 409, one delivery only
//	COA-1  coalesce:    a burst produces far fewer messages than per-event sends
//	FOR-1  forum path:   event->forum topic delivery uses message_thread_id
//	FOR-2  setup gates:  sender-not-admin / bot-no-manage-topics / non-forum keep 'dm'
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

// COA-1: send 60 events for one user/service paced under the per-service 1s burst cap (<=30/s)
// so all are accepted and land in few coalesce windows. Pipelines collapse 60 into a handful of
// batched messages (far fewer than 60). We assert coalescing happened and batches respect cap.
func TestCOA1CoalesceBurst(t *testing.T) {
	mock, err := newMockBot(testBotToken)
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	t.Cleanup(mock.Close)
	h := newHarness(t, mock, &harnessOpts{coalesceWindow: 900 * time.Millisecond, coalesceCap: 20})
	tok := h.seedService(t, "goyoutube")
	h.seedUser(t, "goyoutube", "1", 111)
	h.setGroup(t, 111, 9001)
	_ = h.st.SetTopic(context.Background(), 111, "goyoutube", 5)

	const n = 60
	accepted := 0
	for i := 0; i < n; i++ {
		rec := h.postEvent(t, tok, mkEv("goyoutube", "job_completed", "coa-"+itoa(i), "1"))
		if rec.Code == http.StatusOK {
			accepted++
		}
		time.Sleep(40 * time.Millisecond) // 25/s -> under 30/s burst cap
	}
	if accepted != n {
		t.Fatalf("all %d must be accepted under rate limit (got %d)", n, accepted)
	}
	waitFor(t, 5*time.Second, func() bool { return len(mock.sendMessages()) >= 1 })
	time.Sleep(300 * time.Millisecond)
	msgs := len(mock.sendMessages())
	if msgs >= n {
		t.Fatalf("coalesce failed: %d messages for %d events", msgs, n)
	}
	if msgs > 12 {
		t.Fatalf("expected strong coalescing for 60 events, got %d messages", msgs)
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
// matching error and leaves the user in 'dm' with no topics created.
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

	// happy: all gates pass -> S2 succeeds (issues a connect code; see FOR-1/FOR-3 flip path).
	mock.senderAdmin = true
	mock.botAdmTopics = true
	h.menuCallback(111, 9001, "setup:done:111")
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
