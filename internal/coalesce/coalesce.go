// Package coalesce implements the per-key 5s tumbling coalescing window (A-8).
// Key is (user_id, service, type). The first event of a key arms a timer; later
// events join the batch; a flush renders ONE message when the timer fires or the batch
// cap (20) is reached.
package coalesce

import (
	"context"
	"sync"
	"time"
)

// Batcher is a callback the coalescer invokes on flush with the batch of events.
type Batcher interface {
	Flush(ctx context.Context, key Key, batch []*Item)
}

// Key identifies a coalescing group.
type Key struct {
	UserID  int64
	Service string
	Type    string
}

// Item is one event waiting in a batch.
type Item struct {
	UserID   int64
	EventID  string
	Severity string
	Title    string
	Text     string
	URL      string
}

// Coalescer groups events by key into tumbling windows.
type Coalescer struct {
	mu     sync.Mutex
	window time.Duration
	cap    int
	now    func() time.Time
	flush  Batcher
	b      map[Key]*pending
}

type pending struct {
	key   Key
	items []*Item
	timer *time.Timer
}

// New returns a coalescer with the given window duration and batch cap. onFlush is
// invoked (in its own goroutine) per flushed batch.
func New(window time.Duration, cap int, onFlush Batcher) *Coalescer {
	return &Coalescer{
		window: window,
		cap:    cap,
		now:    time.Now,
		flush:  onFlush,
		b:      make(map[Key]*pending),
	}
}

// Add places an event into its key's current batch. When it is the first event of the
// key it arms the tumbling timer.
func (c *Coalescer) Add(ctx context.Context, key Key, item *Item) {
	c.mu.Lock()
	p, ok := c.b[key]
	if !ok {
		p = &pending{key: key}
		p.timer = time.AfterFunc(c.window, func() {
			c.mu.Lock()
			if p == c.b[key] {
				delete(c.b, key)
			}
			c.mu.Unlock()
			c.flushBatch(ctx, p)
		})
		c.b[key] = p
	}
	p.items = append(p.items, item)
	hitCap := len(p.items) >= c.cap
	c.mu.Unlock()

	if hitCap {
		c.ForceFlush(key, p)
	}
}

// ForceFlush immediately flushes any pending batch for key (used for deterministic tests and
// the batch-cap early flush).
func (c *Coalescer) ForceFlush(key Key, p *pending) {
	c.mu.Lock()
	if c.b[key] != p {
		c.mu.Unlock()
		return
	}
	delete(c.b, key)
	p.timer.Stop()
	c.mu.Unlock()
	c.flushBatch(context.Background(), p)
}

func (c *Coalescer) flushBatch(ctx context.Context, p *pending) {
	if len(p.items) == 0 {
		return
	}
	c.flush.Flush(ctx, p.key, p.items)
}

// Window returns the configured window duration.
func (c *Coalescer) Window() time.Duration { return c.window }
