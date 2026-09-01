// integration harness: builds the full tgNtfy pipeline against the mock Bot API and
// exposes an httptest HTTP surface for POST /v1/events, /v1/link and /api/health.
package itest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/spluft/tgNtfy/internal/catalog"
	"github.com/spluft/tgNtfy/internal/coalesce"
	"github.com/spluft/tgNtfy/internal/healthz"
	"github.com/spluft/tgNtfy/internal/ingest"
	"github.com/spluft/tgNtfy/internal/menu"
	"github.com/spluft/tgNtfy/internal/store"
	"github.com/spluft/tgNtfy/internal/tgbot"
	"github.com/spluft/tgNtfy/internal/transport"
)

// harness wires the gate exactly as main.go does, with the Bot API pointed at the mock.
type harness struct {
	t    *testing.T
	st   *store.Store
	ing  *ingest.Handler
	menu *menu.Handler
	disp *transport.TelegramTransport
	h    *healthz.Health
	bot  *tgbot.Client
	cat  *catalog.Lookup
	// mux routes /v1/ and /api/health
	httpHandler http.Handler
}

type harnessOpts struct {
	coalesceWindow time.Duration
	coalesceCap    int
	dbPath         string
	cat            *catalog.Catalog
}

func testCatalog() *catalog.Catalog {
	c, err := catalog.Load(filepath.Join("..", "..", "config", "events.yaml"))
	if err != nil {
		// fall back to an inline minimal catalog
		c = &catalog.Catalog{Version: 1, Services: map[string]catalog.Service{
			"goyoutube": {DisplayName: "goYouTube", Events: map[string]catalog.EventType{"job_completed": {Severity: "success"}}},
			"gomail":    {DisplayName: "Mail", Events: map[string]catalog.EventType{"new-mail": {Severity: "info"}}},
		}}
	}
	return c
}

func newHarness(t *testing.T, mock *mockBot, o *harnessOpts) *harness {
	t.Helper()
	if o == nil {
		o = &harnessOpts{coalesceWindow: 150 * time.Millisecond, coalesceCap: 20}
	}
	c := o.cat
	if c == nil {
		c = testCatalog()
	}
	catLk := catalog.NewLookup(c)

	dbp := o.dbPath
	if dbp == "" {
		dbp = filepath.Join(t.TempDir(), "itest.db")
	}
	st, err := store.New(dbp)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	client, err := tgbot.New("12345:test", mock.URL(), nil)
	if err != nil {
		t.Fatalf("tgbot: %v", err)
	}

	queue := transport.NewQueue(5000)
	discardLog := slog.New(slog.NewTextHandler(io.Discard, nil))
	disp := transport.NewTelegramTransport(client.Bot, st, queue, discardLog)
	go disp.Run()
	t.Cleanup(disp.Stop)

	// topicResolver mirrors main.go: create (or look up) the forum topic for (user,service).
	resolver := func(cm context.Context, userID, chatID int64, svc string) (int, error) {
		dispName := svc
		if s, ok := c.Services[svc]; ok && s.DisplayName != "" {
			dispName = s.DisplayName
		}
		tid, err := client.CreateTopic(cm, chatID, dispName)
		if err != nil {
			return 0, err
		}
		_ = st.SetTopic(context.Background(), userID, svc, tid)
		return tid, nil
	}

	flusher := &flusher{store: st, enqueue: func(d transport.Delivery) { queue.Enqueue(d) }, cat: c}
	co := coalesce.New(o.coalesceWindow, o.coalesceCap, flusher)

	ing := ingest.NewHandler(st, catLk, nil,
		ingest.WithTopicResolver(resolver),
		ingest.WithCoalescer(co),
	)

	// Top-level mux akin to main.go.
	mux := http.NewServeMux()
	hh := &healthz.Health{PingFunc: st.Ping}
	mux.HandleFunc("/api/health", hh.HandleHealth(""))
	mux.Handle("/v1/", ing.Routes())

	mh := menu.NewHandler(client, st, catLk, nil)
	return &harness{
		t: t, st: st, ing: ing, disp: disp, bot: client, cat: catLk, h: hh, menu: mh,
		httpHandler: mux,
	}
}

// flusher is the Batcher that renders a coalesced batch -> one delivery row -> enqueue.
type flusher struct {
	store   *store.Store
	enqueue func(transport.Delivery)
	cat     *catalog.Catalog
}

// Flush implements coalesce.Batcher: resolves the user's destination chat/thread and
// enqueues a single (possibly batched) delivery.
func (f *flusher) Flush(ctx context.Context, key coalesce.Key, items []*coalesce.Item) {
	if len(items) == 0 {
		return
	}
	first := items[0]
	chatID, threadID := first.UserID, 0
	if u, err := f.store.GetUser(ctx, first.UserID); err == nil && u != nil && u.DeliveryMode == "group" && u.GroupChatID != nil {
		chatID = *u.GroupChatID
		var tid int
		_ = f.store.QueryRow(ctx, "SELECT message_thread_id FROM group_topics WHERE user_id=? AND service=?", first.UserID, key.Service).Scan(&tid)
		threadID = tid
	}
	sev := "info"
	dispName := key.Service
	if s, ok := f.cat.Services[key.Service]; ok && s.DisplayName != "" {
		dispName = s.DisplayName
	}
	text := ingest.RenderMessage(sev, dispName, key.Type, first.Title, first.Text, first.URL)
	if len(items) > 1 {
		text = ingest.RenderBatch(sev, dispName, key.Type, items)
	}
	rowID, err := f.store.CreateDelivery(ctx, first.UserID, chatID, threadID, first.EventID, key.Service, key.Type, len(items))
	if err != nil {
		return
	}
	f.enqueue(transport.Delivery{
		RowID: rowID, UserID: first.UserID, ChatID: chatID,
		MessageThreadID: threadID, Text: text, Service: key.Service, BatchSize: len(items),
	})
}

// postEvent mimics a service POSTing an event to /v1/events with a token.
func (h *harness) postEvent(t *testing.T, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	bs, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/events", bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", token)
	rec := httptest.NewRecorder()
	h.httpHandler.ServeHTTP(rec, req)
	return rec
}

// seedService registers a service with a raw token and returns it.
func (h *harness) seedService(t *testing.T, svc string) string {
	t.Helper()
	tok := "raw-token-" + svc
	_ = h.st.CreateService(context.Background(), svc, svc, tok)
	return tok
}

// seedUser links service user_ref -> tg user and returns nothing.
func (h *harness) seedUser(t *testing.T, svc, userRef string, tgUser int64) {
	t.Helper()
	if err := h.st.UpsertUser(context.Background(), tgUser, "u"+itoa(int(tgUser)), "U"); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	if err := h.st.EnsureServiceUser(context.Background(), svc, userRef, tgUser); err != nil {
		t.Fatalf("ensure service_user: %v", err)
	}
}

// setGroupDelivery sets a user into group mode with a chat id.
func (h *harness) setGroup(t *testing.T, tgUser, groupChat int64) {
	t.Helper()
	gc := groupChat
	if err := h.st.SetDeliveryMode(context.Background(), tgUser, "group", &gc); err != nil {
		t.Fatalf("set delivery mode: %v", err)
	}
}

// update simulating any prefix-typed bot message routed to the menu handler.
func (h *harness) menuUpdate(from int64, text string, chat models.Chat) {
	u := &models.Update{
		Message: &models.Message{
			ID: 1, From: &models.User{ID: from, FirstName: "U"},
			Chat: chat, Text: text,
		},
	}
	h.menu.DefaultHandler(context.Background(), h.bot.Bot, u)
}

// menuCallback delivers a CallbackQuery update (e.g. "setup:done:<uid>" for S2 verify).
func (h *harness) menuCallback(from int64, chatID int64, data string) {
	cq := &models.CallbackQuery{
		ID:   "cb1",
		From: models.User{ID: from, FirstName: "U"},
		Message: models.MaybeInaccessibleMessage{Message: &models.Message{
			ID: 1, Chat: models.Chat{ID: chatID, Type: models.ChatTypePrivate},
		}},
		Data: data,
	}
	u := &models.Update{CallbackQuery: cq}
	h.menu.DefaultHandler(context.Background(), h.bot.Bot, u)
}
