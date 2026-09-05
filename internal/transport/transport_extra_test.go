package transport

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-telegram/bot"
)

func TestRetry429DoesNotConsumeAttempt(t *testing.T) {
	fn := &fakeStore{}
	s := &fakeSender{fail: pattern{errs: []error{
		&bot.TooManyRequestsError{RetryAfter: 1},
		errors.New("500"),
	}}}
	tt := NewTelegramTransport(s, fn, NewQueue(1), testLogger())
	tt.chatPace = NewTokenBucket(1000, 1000)
	tt.sleep = func(time.Duration) {}
	err := tt.SendNow(context.Background(), Delivery{RowID: 9})
	if err != nil {
		t.Fatal(err)
	}
	if fn.ret429 != 1 {
		t.Fatalf("expected 1 DeliveryRetry429, got %d", fn.ret429)
	}
	if fn.sent != 1 {
		t.Fatalf("expected 1 sent, got %d", fn.sent)
	}
	if fn.failed != 1 {
		t.Fatalf("expected 1 failed (the 500), got %d", fn.failed)
	}
}

func TestQueueDropsWhenFull(t *testing.T) {
	q := NewQueue(2)
	q.Enqueue(Delivery{RowID: 1})
	q.Enqueue(Delivery{RowID: 2})
	q.Enqueue(Delivery{RowID: 3}) // full -> dropped
	stop := make(chan struct{})
	d1, _ := q.recvBlocking(stop)
	d2, _ := q.recvBlocking(stop)
	close(stop) // drain stops -> recvBlocking returns (false) without blocking
	if _, ok := q.recvBlocking(stop); ok {
		t.Fatal("queue must be empty; third delivery was dropped")
	}
	if d1.RowID != 1 || d2.RowID != 2 {
		t.Fatalf("queue order: d1=%d d2=%d", d1.RowID, d2.RowID)
	}
}
