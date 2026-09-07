package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	st, err := New(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestLinkIdentityErrors(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	st.UpsertUser(ctx, 10, "a", "A")
	st.UpsertUser(ctx, 11, "b", "B")
	st.CreateService(ctx, "govpn", "VPN", "tok")
	st.CreateLinkCode(ctx, "c1", "govpn", 10, 10*time.Minute, nil)

	if err := st.LinkIdentity(ctx, "govpn", "r1", "c1", 10, 11, ""); err != ErrCodeMismatch {
		t.Fatalf("user mismatch: want ErrCodeMismatch, got %v", err)
	}
	st.CreateLinkCode(ctx, "c2", "govpn", 10, 10*time.Minute, nil)
	if err := st.LinkIdentity(ctx, "govpn", "r2", "c2", 10, 10, ""); err != nil {
		t.Fatalf("first link: %v", err)
	}
	// a DIFFERENT tg user (12) tries to bind the same user_ref r2 -> ErrAlreadyLinked
	st.UpsertUser(ctx, 12, "c2u", "C2")
	st.CreateLinkCode(ctx, "c3", "govpn", 12, 10*time.Minute, nil)
	if err := st.LinkIdentity(ctx, "govpn", "r2", "c3", 12, 12, ""); err != ErrAlreadyLinked {
		t.Fatalf("already linked: want ErrAlreadyLinked, got %v", err)
	}
	// expired code -> ErrCodeInvalid via UserIDForLinkCode
	st.CreateLinkCode(ctx, "expired", "govpn", 10, -time.Minute, nil)
	if _, err := st.UserIDForLinkCode(ctx, "expired"); err != ErrCodeInvalid {
		t.Fatalf("expired: want ErrCodeInvalid, got %v", err)
	}
	if _, err := st.UserIDForLinkCode(ctx, "nope"); err != ErrCodeInvalid {
		t.Fatalf("unknown: want ErrCodeInvalid, got %v", err)
	}
}

func TestConnectCodeExpiryAndInvalid(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	st.UpsertUser(ctx, 20, "c", "C")
	st.CreateConnectCode(ctx, "abc", 20, -time.Minute)
	if err := st.ConsumeConnectCode(ctx, "abc", 20); err != ErrCodeInvalid {
		t.Fatalf("expired connect code: want ErrCodeInvalid, got %v", err)
	}
	if err := st.ConsumeConnectCode(ctx, "zzz", 20); err != ErrCodeInvalid {
		t.Fatalf("unknown connect code: want ErrCodeInvalid, got %v", err)
	}
}

func TestEnsureTopicAndGetThread(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	st.UpsertUser(ctx, 30, "d", "D")
	st.CreateTokenlessService(ctx, "gomail", "Mail")

	tid, created, err := st.EnsureTopic(ctx, 30, 100, "gomail",
		func(int64, string) (int, error) { return 42, nil })
	if err != nil || !created || tid != 42 {
		t.Fatalf("first EnsureTopic: tid=%d created=%v err=%v", tid, created, err)
	}
	tid2, created2, _ := st.EnsureTopic(ctx, 30, 100, "gomail",
		func(int64, string) (int, error) { return 999, nil })
	if created2 || tid2 != 42 {
		t.Fatalf("re-EnsureTopic: tid=%d created=%v want 42/false", tid2, created2)
	}
	if tt, found, err := st.GetTopicThread(ctx, 30, "gomail"); err != nil || !found || tt != 42 {
		t.Fatalf("GetTopicThread: tt=%d found=%v err=%v", tt, found, err)
	}
	if _, found, _ := st.GetTopicThread(ctx, 31, "gomail"); found {
		t.Fatal("GetTopicThread for unknown user should be not-found")
	}
	if _, _, err := st.EnsureTopic(ctx, 30, 100, "bogus",
		func(int64, string) (int, error) { return 0, errors.New("boom") }); err == nil {
		t.Fatal("creator error should propagate")
	}
}

func TestRecentEventsAndBatchStats(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	st.UpsertUser(ctx, 40, "e", "E")
	st.CreateTokenlessService(ctx, "govpn", "VPN")
	e := &Event{EventID: "e1", Service: "govpn", UserRef: "1", Type: "vpn_connected",
		Severity: "success", Title: "connected", ReceivedAt: time.Now()}
	st.InsertEvent(ctx, e)
	st.CreateDelivery(ctx, 40, 7, 3, "e1", "govpn", "vpn_connected", 2)
	recs, err := st.RecentEvents(ctx, 10)
	if err != nil || len(recs) != 1 || recs[0].Title != "connected" {
		t.Fatalf("RecentEvents: %+v err=%v", recs, err)
	}
	sum, maxB, err := st.DeliveryBatchStats(ctx, 40, "govpn")
	if err != nil || sum != 2 || maxB != 2 {
		t.Fatalf("Stats: sum=%d max=%d err=%v", sum, maxB, err)
	}
	last, lt, err := st.LastEventForUserService(ctx, 40, "govpn")
	if err != nil || last != "connected" || lt.IsZero() {
		t.Fatalf("LastEvent: %s %v err=%v", last, lt, err)
	}
	if err := st.ClearUserTopics(ctx, 40); err != nil {
		t.Fatal(err)
	}
	if dn, _ := st.DisplayName(ctx, "nonexistent"); dn != "nonexistent" {
		t.Fatalf("DisplayName fallback: %q", dn)
	}
}

func TestUserListWithGroupChat(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	st.UpsertUser(ctx, 1, "u1", "U1")
	st.UpsertUser(ctx, 2, "u2", "U2")
	gc := int64(5)
	st.SetDeliveryMode(ctx, 1, "group", &gc)
	users, err := st.UserList(ctx)
	if err != nil || len(users) != 2 {
		t.Fatalf("UserList: %+v err=%v", users, err)
	}
	if users[0].GroupChatID == nil || *users[0].GroupChatID != 5 {
		t.Fatalf("user1 group chat: %+v", users[0])
	}
}
