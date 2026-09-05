// Package dispatch owns the coalesce-flush and lazy topic-resolver wiring that the
// server (cmd/tgntfy) and the integration harness share. Extracted from main.go by
// epic t_2d992300 (R1) so the itest suite exercises REAL production flush logic
// instead of a near-identical copy. Behavior-preserving: bodies moved verbatim.
package dispatch

import (
	"context"
	"log/slog"

	"github.com/spluft/tgNtfy/internal/coalesce"
	"github.com/spluft/tgNtfy/internal/ingest"
	"github.com/spluft/tgNtfy/internal/store"
	"github.com/spluft/tgNtfy/internal/topic"
	"github.com/spluft/tgNtfy/internal/transport"
)

// TopicCreator is the narrow Bot API surface the resolver needs (satisfied by
// *tgbot.Client), kept as an interface so dispatch does not import tgbot.
type TopicCreator interface {
	CreateTopic(ctx context.Context, chatID any, name string) (int, error)
}

// TopicResolverFor builds the lazy topic resolver: createForumTopic only on a
// group_topics miss (the store's EnsureTopic does the row-first lookup + idempotent
// row upsert). Name source is the store's services.display_name (V-1/V-5), never the
// catalog; fallback: service id.
func TopicResolverFor(st *store.Store, creator TopicCreator) store.TopicResolver {
	return func(cm context.Context, userID, chatID int64, svc string) (int, error) {
		disp, _ := st.DisplayName(cm, svc)
		if name := topic.SanitizeTopicName(disp); name != "" {
			disp = name
		}
		return creator.CreateTopic(cm, chatID, disp)
	}
}

// BatchFlusher renders a coalesced batch and enqueues it as one delivery row.
// It implements coalesce.Batcher.
type BatchFlusher struct {
	enqueue func(ctx context.Context, d transport.Delivery)
	store   *store.Store
	log     *slog.Logger
}

// NewBatchFlusher wires a flusher over the store with an enqueue callback (dispatcher
// or queue). A nil log falls back to slog.Default().
func NewBatchFlusher(st *store.Store, enqueue func(ctx context.Context, d transport.Delivery), log *slog.Logger) *BatchFlusher {
	if log == nil {
		log = slog.Default()
	}
	return &BatchFlusher{enqueue: enqueue, store: st, log: log}
}

// Flush resolves the destination chat/thread, renders one message for the batch,
// creates the delivery row and enqueues it (moved verbatim from main.go).
func (b *BatchFlusher) Flush(ctx context.Context, key coalesce.Key, items []*coalesce.Item) {
	if len(items) == 0 {
		return
	}
	ev := items[0]
	// Resolve the destination chat + thread for this user.
	chatID, threadID := ev.UserID, 0
	if u, err := b.store.GetUser(ctx, ev.UserID); err == nil && u != nil && u.DeliveryMode == "group" && u.GroupChatID != nil {
		chatID = *u.GroupChatID
		if tid, found, _ := b.store.GetTopicThread(ctx, ev.UserID, key.Service); found {
			threadID = tid
		}
	}
	disp, _ := b.store.DisplayName(ctx, key.Service)
	text := ingest.RenderMessage(ev.Severity, disp, key.Type, ev.Title, ev.Text, ev.URL)
	if len(items) > 1 {
		text = ingest.RenderBatch(ev.Severity, disp, key.Type, items)
	}
	rowID, err := b.store.CreateDelivery(ctx, ev.UserID, chatID, threadID, ev.EventID, key.Service, key.Type, len(items))
	if err != nil {
		b.log.Error("create delivery row", "err", err)
		return
	}
	b.enqueue(ctx, transport.Delivery{
		RowID:           rowID,
		UserID:          ev.UserID,
		ChatID:          chatID,
		MessageThreadID: threadID,
		Text:            text,
		Service:         key.Service,
	})
	ingest.RecordBatch(len(items))
}
