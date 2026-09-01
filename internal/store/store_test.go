package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := New(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	tok := "rawtoken"
	if err := st.CreateService(ctx, "goyoutube", "goYouTube", tok); err != nil {
		t.Fatal(err)
	}
	// Only the hash is stored, not the raw.
	svc, ok := st.VerifyToken(ctx, "rawtoken")
	if !ok || svc != "goyoutube" {
		t.Fatalf("VerifyToken: %v %v", svc, ok)
	}
	if _, ok := st.VerifyToken(ctx, "wrong"); ok {
		t.Fatal("wrong token must not verify")
	}
	h := TokenHash("rawtoken")
	if h == "rawtoken" || len(h) != 64 {
		t.Fatalf("token hash bad: %q", h)
	}
	// idempotency via InsertEvent
	e := &Event{EventID: "e1", Service: "goyoutube", UserRef: "1", Type: "job_completed",
		Severity: "success", Title: "T", ReceivedAt: time.Now()}
	if err := st.InsertEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertEvent(ctx, e); err != ErrDuplicateEvent {
		t.Fatalf("dup: want ErrDuplicateEvent, got %v", err)
	}
}

func TestRoutingIsolation(t *testing.T) {
	ctx := context.Background()
	st, _ := New(filepath.Join(t.TempDir(), "iso.db"))
	defer st.Close()

	// Two users.
	st.EnsureRegistered(ctx, "goyoutube", "goYouTube")
	st.UpsertUser(ctx, 1001, "ua", "Ua")
	st.UpsertUser(ctx, 1002, "ub", "Ub")
	// user A gets user_ref=1, user B gets user_ref=2 for goyoutube.
	st.EnsureServiceUser(ctx, "goyoutube", "1", 1001)
	st.EnsureServiceUser(ctx, "goyoutube", "2", 1002)

	// user A set up their own group.
	gc := int64(900001)
	st.SetDeliveryMode(ctx, 1001, "group", &gc)

	e := &Event{EventID: "iso1", Service: "goyoutube", UserRef: "1", Type: "job_completed",
		Severity: "success", Title: "A event", ReceivedAt: time.Now()}
	if err := st.InsertEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	routes, err := st.ResolveRoutes(ctx, e, func(ctx context.Context, u, c int64, s string) (int, error) { return 42, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("expected exactly 1 route (only user A), got %d", len(routes))
	}
	if routes[0].UserID != 1001 || routes[0].ChatID != gc {
		t.Fatalf("route wrong: %+v", routes[0])
	}
}

func TestRoutingSubFilter(t *testing.T) {
	ctx := context.Background()
	st, _ := New(filepath.Join(t.TempDir(), "filter.db"))
	defer st.Close()
	st.UpsertUser(ctx, 5, "u", "U")
	st.EnsureRegistered(ctx, "gomail", "Mail")
	st.EnsureServiceUser(ctx, "gomail", "17", 5)
	// user enabled only new-mail, not sync-status.
	st.SetEventTypes(ctx, 5, "gomail", []string{"new-mail"})
	e := &Event{EventID: "m1", Service: "gomail", UserRef: "17", Type: "sync-status",
		Severity: "warn", Title: "sync down", ReceivedAt: time.Now()}
	st.InsertEvent(ctx, e)
	routes, _ := st.ResolveRoutes(ctx, e, nil)
	if len(routes) != 0 {
		t.Fatalf("expected 0 routes (sync-status filtered), got %d", len(routes))
	}
	// muted also suppresses.
	e2 := &Event{EventID: "m2", Service: "gomail", UserRef: "17", Type: "new-mail",
		Severity: "info", Title: "new", ReceivedAt: time.Now()}
	st.InsertEvent(ctx, e2)
	st.SetMuted(ctx, 5, "gomail", true)
	routes2, _ := st.ResolveRoutes(ctx, e2, nil)
	if len(routes2) != 0 {
		t.Fatalf("expected 0 routes (muted), got %d", len(routes2))
	}
}

func TestConnectCodeSingleUse(t *testing.T) {
	ctx := context.Background()
	st, _ := New(filepath.Join(t.TempDir(), "cc.db"))
	defer st.Close()
	st.UpsertUser(ctx, 9, "u", "U")
	if err := st.CreateConnectCode(ctx, "111111", 9, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := st.ConsumeConnectCode(ctx, "111111", 9); err != nil {
		t.Fatal(err)
	}
	if err := st.ConsumeConnectCode(ctx, "111111", 9); err != ErrCodeUsed {
		t.Fatalf("second consume: want ErrCodeUsed, got %v", err)
	}
}

func TestDeliveryLifecycle(t *testing.T) {
	ctx := context.Background()
	st, _ := New(filepath.Join(t.TempDir(), "dl.db"))
	defer st.Close()
	st.UpsertUser(ctx, 3, "u", "U")
	st.CreateTokenlessService(ctx, "govpn", "VPN")
	id, err := st.CreateDelivery(ctx, 3, 7, 5, "govpn:v1", "govpn", "vpn_connected", 1)
	if err != nil || id == 0 {
		t.Fatalf("create delivery: %v %d", err, id)
	}
	if err := st.MarkDeliverySent(ctx, id, 999999); err != nil {
		t.Fatal(err)
	}
	fails, _ := st.FailedDeliveries(ctx, 10)
	if len(fails) != 0 {
		t.Fatalf("expected 0 failed, got %d", len(fails))
	}
	if err := st.FailDeliveryPermanently(ctx, id, 5, "boom"); err != nil {
		t.Fatal(err)
	}
	fails, _ = st.FailedDeliveries(ctx, 10)
	if len(fails) != 1 {
		t.Fatalf("expected 1 failed, got %d", len(fails))
	}
	n, _ := st.RetryAllFailed(ctx, 50)
	if n != 1 {
		t.Fatalf("expected 1 retried, got %d", n)
	}
}
