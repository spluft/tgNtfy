// Package transport implements the Dispatcher interface (the extension point for future
// non-Telegram channels) backed by a Telegram sender. It owns the bounded per-chat
// delivery queue, egress pacing (token bucket), and the A-10 exponential backoff retry
// with the store's delivery-row bookkeeping and TG 429 honouring.
package transport

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Delivery is the immutable unit of a dispatcher send, created at coalesce-flush time.
type Delivery struct {
	RowID           int64 // deliveries.id
	UserID          int64
	ChatID          int64
	MessageThreadID int
	Text            string
	Service         string
	BatchSize       int
}

// Dispatcher is the delivery abstraction. Future channels implement this same interface.
type Dispatcher interface {
	Enqueue(d Delivery)
	SendNow(ctx context.Context, d Delivery) error
}

// StoreIface is the delivery-row bookkeeping the dispatcher needs.
type StoreIface interface {
	MarkDeliverySent(ctx context.Context, id, tgMsgID int64) error
	DeliveryAttemptFailed(ctx context.Context, id int64, attempts int, nextRetry *time.Time, errMsg string) error
	DeliveryRetry429(ctx context.Context, id int64) error
	DeliveryExhausted(ctx context.Context, id int64, attempts int, errMsg string) error
}

var (
	deliveriesOK = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "deliveries_ok_total", Help: "Successful TG sends.",
	}, []string{"service"})
	deliveriesFail = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "deliveries_fail_total", Help: "Exhausted/final delivery failures.",
	}, []string{"service"})
	deliveries429 = promauto.NewCounter(prometheus.CounterOpts{
		Name: "deliveries_429_total", Help: "TG flood-limit deferrals.",
	})
	queueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "queue_depth", Help: "Total pending deliveries across chat queues.",
	})
)

// Sender is the narrow Bot API send surface (mockable in tests).
type Sender interface {
	SendMessage(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error)
}

// maxAttempts and base backoff per A-10 (5s × 2^n, cap 5min).
const maxAttempts = 5

// backoff returns the wait for attempt n (0-indexed): 5s × 2^n, capped at 5 minutes.
func backoff(n int) time.Duration {
	d := time.Duration(5) * time.Second * (1 << n)
	if d > 5*time.Minute {
		return 5 * time.Minute
	}
	return d
}

// TokenBucket paces egress to the per-chat ~30 msg/min and global 15/s caps.
type TokenBucket struct {
	capacity  float64
	refill43s float64 // tokens per second
	current   float64
	last      time.Time
	now       func() time.Time
}

// NewTokenBucket builds a bucket with `capacity` tokens refilling at `perSec`/s.
func NewTokenBucket(capacity, perSec float64) *TokenBucket {
	return &TokenBucket{
		capacity: capacity, refill43s: perSec, current: capacity,
		last: time.Now(), now: time.Now,
	}
}

// take blocks until a token is available.
func (t *TokenBucket) take() {
	for {
		now := t.now()
		dt := now.Sub(t.last).Seconds()
		t.current += dt * t.refill43s
		if t.current > t.capacity {
			t.current = t.capacity
		}
		t.last = now
		if t.current >= 1 {
			t.current--
			return
		}
		need := time.Duration((1 - t.current) / t.refill43s * float64(time.Second))
		time.Sleep(need)
	}
}

// Queue is a bounded FIFO for Deliveries.
type Queue struct {
	ch chan Delivery
}

// NewQueue returns a queue with the given capacity.
func NewQueue(capacity int) *Queue { return &Queue{ch: make(chan Delivery, capacity)} }

// Enqueue pushes a delivery (non-blocking; drops on a full queue).
func (q *Queue) Enqueue(d Delivery) {
	select {
	case q.ch <- d:
		queueDepth.Set(float64(len(q.ch)))
	default:
	}
}

// TryRecv attempts a non-blocking receive.
func (q *Queue) TryRecv() (Delivery, bool) {
	select {
	case d := <-q.ch:
		queueDepth.Set(float64(len(q.ch)))
		return d, true
	default:
		return Delivery{}, false
	}
}

func (q *Queue) recvBlocking(stop <-chan struct{}) (Delivery, bool) {
	select {
	case d := <-q.ch:
		queueDepth.Set(float64(len(q.ch)))
		return d, true
	case <-stop:
		return Delivery{}, false
	}
}

// TelegramTransport implements Dispatcher with retry + pacing + row bookkeeping.
type TelegramTransport struct {
	bot      Sender
	store    StoreIface
	log      *slog.Logger
	queue    *Queue
	chatPace *TokenBucket // 30 msg/min per chat => shared global approximation
	stop     chan struct{}
	sleep    func(d time.Duration)
}

// NewTelegramTransport wires a sender, store, and bounded queue with egress pacing.
func NewTelegramTransport(b Sender, st StoreIface, queue *Queue, log *slog.Logger) *TelegramTransport {
	tt := &TelegramTransport{
		bot: b, store: st, log: log.With("component", "transport"),
		queue:    queue,
		chatPace: NewTokenBucket(30, 0.5),
		stop:     make(chan struct{}),
		sleep:    time.Sleep,
	}
	return tt
}

// Enqueue implements Dispatcher.
func (t *TelegramTransport) Enqueue(d Delivery) { t.queue.Enqueue(d) }

// SendNow sends immediately with the full retry loop (test/direct path).
func (t *TelegramTransport) SendNow(ctx context.Context, d Delivery) error {
	return t.deliverWithRetry(ctx, d)
}

// Run processes the queue until Stop is called.
func (t *TelegramTransport) Run() {
	for {
		d, ok := t.queue.recvBlocking(t.stop)
		if !ok {
			return
		}
		if err := t.deliverWithRetry(context.Background(), d); err != nil {
			t.log.Warn("delivery failed after retries", "row", d.RowID, "err", err)
		}
	}
}

// Stop signals the Run loop to exit (after draining).
func (t *TelegramTransport) Stop() { close(t.stop) }

// deliverWithRetry performs the A-10 retry loop for one delivery.
func (t *TelegramTransport) deliverWithRetry(ctx context.Context, d Delivery) error {
	for attempt := 0; attempt < maxAttempts; attempt++ {
		t.chatPace.take()
		err := t.deliver(ctx, d, attempt)
		if err == nil {
			return nil
		}
		var tre *bot.TooManyRequestsError
		if errors.As(err, &tre) {
			// 429: honor retry_after, do NOT consume an attempt.
			_ = t.store.DeliveryRetry429(ctx, d.RowID)
			wait := time.Duration(tre.RetryAfter) * time.Second
			if wait <= 0 {
				wait = backoff(attempt)
			}
			t.sleep(wait)
			attempt-- // don't advance towards exhaustion
			continue
		}
		if errors.Is(err, bot.ErrorForbidden) {
			// E-9: fail fast after attempt 1.
			_ = t.store.DeliveryExhausted(ctx, d.RowID, attempt+1, err.Error())
			deliveriesFail.WithLabelValues(d.Service).Inc()
			return err
		}
		// transport/5xx/400-class: consume an attempt, backoff.
		nr := time.Now().Add(backoff(attempt))
		_ = t.store.DeliveryAttemptFailed(ctx, d.RowID, attempt+1, &nr, err.Error())
		t.sleep(backoff(attempt))
	}
	_ = t.store.DeliveryExhausted(ctx, d.RowID, maxAttempts, "retries exhausted")
	deliveriesFail.WithLabelValues(d.Service).Inc()
	return errors.New("retries exhausted")
}

func (t *TelegramTransport) deliver(ctx context.Context, d Delivery, attempt int) error {
	res, err := t.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          d.ChatID,
		MessageThreadID: d.MessageThreadID,
		Text:            d.Text,
	})
	if err != nil {
		return err
	}
	_ = t.store.MarkDeliverySent(ctx, d.RowID, int64(res.ID))
	deliveriesOK.WithLabelValues(d.Service).Inc()
	return nil
}
