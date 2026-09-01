package transport

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type fakeStore struct {
	sent, failed, exhausted, ret429 int
	sendErr                         string // optional injected send failure
	sendErrSkip                     int    // first N SendMessage calls succeed
}

func (f *fakeStore) MarkDeliverySent(ctx context.Context, id, tgMsgID int64) error {
	f.sent++
	return nil
}
func (f *fakeStore) DeliveryAttemptFailed(ctx context.Context, id int64, a int, next *time.Time, e string) error {
	f.failed++
	return nil
}
func (f *fakeStore) DeliveryRetry429(ctx context.Context, id int64) error { f.ret429++; return nil }
func (f *fakeStore) DeliveryExhausted(ctx context.Context, id int64, a int, e string) error {
	f.exhausted++
	return nil
}

type fakeSender struct {
	fail pattern
}

// pattern controls per-call SendMessage results.
type pattern struct {
	errs []error // one result per call; exhausted calls reuse last
}

func (f *fakeSender) SendMessage(ctx context.Context, p *bot.SendMessageParams) (*models.Message, error) {
	if len(f.fail.errs) > 0 {
		e := f.fail.errs[0]
		f.fail.errs = f.fail.errs[1:]
		return nil, e
	}
	return &models.Message{ID: 1}, nil
}

func TestBackoffSequence(t *testing.T) {
	want := []time.Duration{5e9, 10e9, 20e9, 40e9, 80e9}
	for i := 0; i < len(want); i++ {
		if got := backoff(i); got != want[i] {
			t.Errorf("backoff(%d) = %s, want %s", i, got, want[i])
		}
	}
	d := backoff(6)
	if d > 5*time.Minute {
		t.Fatalf("backoff must cap at 5min, got %s", d)
	}
}

func TestForbiddenFailsFast(t *testing.T) {
	fn := &fakeStore{}
	s := &fakeSender{fail: pattern{errs: []error{bot.ErrorForbidden}}}
	tt := NewTelegramTransport(s, fn, NewQueue(1), testLogger())
	tt.chatPace = NewTokenBucket(1000, 1000) // no pacing delay
	tt.sleep = func(time.Duration) {}
	err := tt.SendNow(context.Background(), Delivery{RowID: 1})
	if err == nil {
		t.Fatal("expected error for forbidden")
	}
	if fn.exhausted != 1 {
		t.Fatalf("expected fail-fast exhaustion, exhausted=%d", fn.exhausted)
	}
}

func TestFailedThenSent(t *testing.T) {
	fn := &fakeStore{}
	s := &fakeSender{fail: pattern{errs: []error{
		errors.New("500"), errors.New("500"), errors.New("500"), errors.New("500"),
	}}}
	tt := NewTelegramTransport(s, fn, NewQueue(1), testLogger())
	tt.chatPace = NewTokenBucket(1000, 1000)
	tt.sleep = func(time.Duration) {}
	err := tt.SendNow(context.Background(), Delivery{RowID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if fn.sent != 1 || fn.failed != 4 || fn.exhausted != 0 {
		t.Fatalf("sent=%d failed=%d exhausted=%d", fn.sent, fn.failed, fn.exhausted)
	}
}

func TestRetriesExhausted(t *testing.T) {
	fn := &fakeStore{}
	s := &fakeSender{fail: pattern{errs: []error{
		errors.New("500"), errors.New("500"), errors.New("500"),
		errors.New("500"), errors.New("500"),
	}}}
	tt := NewTelegramTransport(s, fn, NewQueue(1), testLogger())
	tt.chatPace = NewTokenBucket(1000, 1000)
	tt.sleep = func(time.Duration) {}
	err := tt.SendNow(context.Background(), Delivery{RowID: 3})
	if err == nil {
		t.Fatal("expected exhausted error")
	}
	if fn.exhausted != 1 {
		t.Fatalf("expected exhausted once, got %d", fn.exhausted)
	}
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
