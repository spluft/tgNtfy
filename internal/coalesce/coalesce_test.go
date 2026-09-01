package coalesce

import (
	"context"
	"sync"
	"testing"
	"time"
)

type rec struct {
	mu    sync.Mutex
	keys  []Key
	count []int
}

func (r *rec) Flush(_ context.Context, k Key, items []*Item) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, k)
	r.count = append(r.count, len(items))
}

// TestBatchCap ensures a batch over cap flushes early (cap=2 -> flush at every 2nd).
func TestBatchCap(t *testing.T) {
	r := &rec{}
	c := New(time.Hour, 2, r) // huge window so the timer never fires; only cap drives flush
	k := Key{UserID: 1, Service: "s", Type: "t"}
	for i := 0; i < 5; i++ {
		c.Add(context.Background(), k, &Item{Title: "x"})
	}
	time.Sleep(10 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	var total int
	for _, n := range r.count {
		total += n
	}
	// items 1,2 flush (caps), 3,4 flush (caps), item 5 stays pending (timer 1h).
	if total != 4 {
		t.Fatalf("expected 4 items flushed by cap, got %d (counts=%v)", total, r.count)
	}
	for _, n := range r.count {
		if n > 2 {
			t.Fatalf("a batch exceeded cap: %d", n)
		}
	}
}

// TestWindowFlush ensures the timer flushes after `window`.
func TestWindowFlush(t *testing.T) {
	r := &rec{}
	c := New(30*time.Millisecond, 100, r)
	k := Key{UserID: 2, Service: "svc", Type: "e"}
	c.Add(context.Background(), k, &Item{Title: "a"})
	c.Add(context.Background(), k, &Item{Title: "b"})
	time.Sleep(80 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.keys) != 1 {
		t.Fatalf("expected 1 flush, got %d", len(r.keys))
	}
	if r.count[0] != 2 {
		t.Fatalf("expected batch of 2, got %d", r.count[0])
	}
}

// TestDifferentKeysAreSeparate confirms (user,service,type) identity isolates keys.
func TestDifferentKeysAreSeparate(t *testing.T) {
	r := &rec{}
	c := New(10*time.Millisecond, 100, r)
	c.Add(context.Background(), Key{UserID: 1, Service: "s", Type: "a"}, &Item{Title: "a"})
	c.Add(context.Background(), Key{UserID: 1, Service: "s", Type: "b"}, &Item{Title: "b"})
	c.Add(context.Background(), Key{UserID: 2, Service: "s", Type: "a"}, &Item{Title: "a2"})
	time.Sleep(80 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.keys) != 3 {
		t.Fatalf("expected 3 separate flushes, got %d", len(r.keys))
	}
}
