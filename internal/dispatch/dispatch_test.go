package dispatch

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spluft/tgNtfy/internal/coalesce"
	"github.com/spluft/tgNtfy/internal/store"
	"github.com/spluft/tgNtfy/internal/transport"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestBatchFlusherSingleAndBatch(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	st.UpsertUser(ctx, 1, "u", "U")
	st.CreateService(ctx, "govpn", "VPN", "tok")
	var enqueued []transport.Delivery
	b := NewBatchFlusher(st, func(_ context.Context, d transport.Delivery) { enqueued = append(enqueued, d) }, slog.Default())

	items := []*coalesce.Item{{UserID: 1, EventID: "e1", Severity: "success", Title: "connected"}}
	b.Flush(ctx, coalesce.Key{UserID: 1, Service: "govpn", Type: "vpn_connected"}, items)
	if len(enqueued) != 1 {
		t.Fatalf("enqueued=%d, want 1", len(enqueued))
	}
	if enqueued[0].ChatID != 1 || enqueued[0].MessageThreadID != 0 {
		t.Fatalf("dm delivery: %+v", enqueued[0])
	}
	if !strings.Contains(enqueued[0].Text, "vpn_connected") {
		t.Fatalf("single render: %q", enqueued[0].Text)
	}

	before := len(enqueued)
	b.Flush(ctx, coalesce.Key{UserID: 1, Service: "govpn"}, nil)
	if len(enqueued) != before {
		t.Fatal("empty batch must not enqueue")
	}

	// A 2-item batch renders the batch format (a per-bullet line) rather than a single
	// message; each coalesced batch is ONE delivery row.
	enqueued = nil
	b.Flush(ctx, coalesce.Key{UserID: 1, Service: "govpn", Type: "vpn_connected"}, []*coalesce.Item{
		{UserID: 1, EventID: "e2", Severity: "warn", Title: "a"},
		{UserID: 1, EventID: "e3", Severity: "warn", Title: "b"},
	})
	if len(enqueued) != 1 {
		t.Fatalf("batch enqueued=%d, want 1", len(enqueued))
	}
	if !strings.Contains(enqueued[0].Text, "2") || !strings.Contains(enqueued[0].Text, "a") {
		t.Fatalf("batch render: %q", enqueued[0].Text)
	}
}

func TestBatchFlusherGroupMode(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	st.UpsertUser(ctx, 2, "u2", "U2")
	st.CreateService(ctx, "gomail", "Mail", "tok")
	gc := int64(9000)
	st.SetDeliveryMode(ctx, 2, "group", &gc)
	st.SetTopic(ctx, 2, "gomail", 505)
	var enqueued []transport.Delivery
	b := NewBatchFlusher(st, func(_ context.Context, d transport.Delivery) { enqueued = append(enqueued, d) }, slog.Default())
	b.Flush(ctx, coalesce.Key{UserID: 2, Service: "gomail", Type: "new-mail"}, []*coalesce.Item{
		{UserID: 2, EventID: "g1", Severity: "info", Title: "hello"},
	})
	if len(enqueued) != 1 {
		t.Fatalf("enqueued=%d", len(enqueued))
	}
	if enqueued[0].ChatID != 9000 || enqueued[0].MessageThreadID != 505 {
		t.Fatalf("group delivery: %+v", enqueued[0])
	}
}

func TestTopicResolverFallback(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := TopicResolverFor(st, &fakeTopicCreator{})
	if tid, err := r(ctx, 1, 100, "nosuchsvc"); err != nil || tid != 7 {
		t.Fatalf("resolver: tid=%d err=%v", tid, err)
	}
}

type fakeTopicCreator struct{}

func (f *fakeTopicCreator) CreateTopic(ctx context.Context, chatID any, name string) (int, error) {
	return 7, nil
}
