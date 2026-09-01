// Command tgntfy is the unified tgNtfy notification gate: HTTP ingest (POST
// /v1/events, /v1/link), Telegram forum-group delivery, self-serve menu, admin CLI,
// healthz + Prometheus metrics.
//
// Server mode requires TG_BOT_TOKEN. `tgntfy admin <db-path> <subcommand>` runs the
// admin CLI against the DB.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spluft/tgNtfy/internal/admin"
	"github.com/spluft/tgNtfy/internal/catalog"
	"github.com/spluft/tgNtfy/internal/coalesce"
	"github.com/spluft/tgNtfy/internal/healthz"
	"github.com/spluft/tgNtfy/internal/ingest"
	"github.com/spluft/tgNtfy/internal/menu"
	"github.com/spluft/tgNtfy/internal/store"
	"github.com/spluft/tgNtfy/internal/tgbot"
	"github.com/spluft/tgNtfy/internal/transport"
)

const defaultCatalogPath = "/etc/tgntfy/events.yaml"

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "admin" {
		admin.Run(os.Args[2:], os.Stdout, os.Stderr)
		return
	}
	if err := runServer(); err != nil {
		slog.Error("tgntfy exited", "err", err)
		os.Exit(1)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func runServer() error {
	level := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	var lh slog.Handler
	if env("LOG_FORMAT", "json") == "text" {
		lh = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	} else {
		lh = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	}
	log := slog.New(lh)
	slog.SetDefault(log)

	if os.Getenv("TG_BOT_TOKEN") == "" {
		return errors.New("TG_BOT_TOKEN is required to run the server (see .env.example)")
	}
	dbPath := env("DB_PATH", "/data/tgntfy.db")
	catalogPath := env("CATALOG_PATH", defaultCatalogPath)
	listenAddr := env("LISTEN_ADDR", ":8080")
	coalesceMs := envInt("COALESCE_WINDOW_MS", 5000)
	adminToken := env("ADMIN_TOKEN", "")

	// Store first (needed by everything).
	st, err := store.New(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// Catalog.
	cat, err := catalog.Load(catalogPath)
	if err != nil {
		log.Warn("catalog load failed; starting with empty catalog", "err", err)
		cat = &catalog.Catalog{Version: 1}
		if cat.Services == nil {
			cat.Services = map[string]catalog.Service{}
		}
	}
	catLk := catalog.NewLookup(cat)
	catLk.SetPath(catalogPath)
	for svc, s := range cat.Services {
		_ = st.EnsureRegistered(context.Background(), svc, s.DisplayName)
	}

	// TG client + menu handler.
	client, err := tgbot.New(os.Getenv("TG_BOT_TOKEN"), os.Getenv("TG_API_URL"), nil)
	if err != nil {
		return err
	}
	menuH := menu.NewHandler(client, st, catLk, log)
	client.SetHandler(menuH.DefaultHandler)

	// Topic resolver: group-delivery topic lookup + lazy createForumTopic.
	topicResolver := func(cm context.Context, userID, chatID int64, svc string) (int, error) {
		disp := svc
		if s, ok := cat.Services[svc]; ok && s.DisplayName != "" {
			disp = s.DisplayName
		}
		tid, err := client.CreateTopic(cm, chatID, disp)
		if err != nil {
			return 0, err
		}
		_ = st.SetTopic(context.Background(), userID, svc, tid)
		return tid, nil
	}

	// Dispatcher.
	queue := transport.NewQueue(5000)
	dispatcher := transport.NewTelegramTransport(client.Bot, st, queue, log)
	go dispatcher.Run()
	defer dispatcher.Stop()

	// Coalescer: flush -> resolve dest + create delivery row + enqueue.
	flusher := &batchFlusher{
		enqueue: func(cm context.Context, d transport.Delivery) { dispatcher.Enqueue(d) },
		store:   st,
		log:     log,
	}
	coalescer := coalesce.New(time.Duration(coalesceMs)*time.Millisecond, 20, flusher)

	// Ingest.
	ing := ingest.NewHandler(st, catLk, log,
		ingest.WithTopicResolver(topicResolver),
		ingest.WithCoalescer(coalescer),
	)

	// Health + metrics + root mux.
	h := &healthz.Health{PingFunc: st.Ping}
	root := http.NewServeMux()
	root.HandleFunc("/api/health", h.HandleHealth(adminToken))
	root.Handle("/metrics", healthz.Metrics())
	root.Handle("/v1/", ing.Routes())

	srv := &http.Server{Addr: listenAddr, Handler: root}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// SIGHUP catalog reload.
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for range sighup {
			if err := catLk.Reload(); err != nil {
				log.Error("catalog reload failed; keeping previous", "err", err)
			}
		}
	}()

	go client.StartBlocking(ctx)

	log.Info("tgntfy listening", "addr", listenAddr, "db", dbPath)
	client.SetMyCommands(ctx)

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal")
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shCtx)
	case err := <-errc:
		return err
	}
}

// batchFlusher renders a coalesced batch and enqueues it as one delivery row.
type batchFlusher struct {
	enqueue func(ctx context.Context, d transport.Delivery)
	store   *store.Store
	log     *slog.Logger
}

func (b *batchFlusher) Flush(ctx context.Context, key coalesce.Key, items []*coalesce.Item) {
	if len(items) == 0 {
		return
	}
	ev := items[0]
	// Resolve the destination chat + thread for this user.
	chatID, threadID := ev.UserID, 0
	if u, err := b.store.GetUser(ctx, ev.UserID); err == nil && u != nil && u.DeliveryMode == "group" && u.GroupChatID != nil {
		chatID = *u.GroupChatID
		var tid int
		er := b.store.QueryRow(ctx, "SELECT message_thread_id FROM group_topics WHERE user_id=? AND service=?", ev.UserID, key.Service).Scan(&tid)
		if er == nil {
			threadID = tid
		}
	}
	text := ingest.RenderMessage(ev.Severity, key.Service, key.Type, ev.Title, ev.Text, ev.URL)
	if len(items) > 1 {
		text = ingest.RenderBatch(ev.Severity, key.Service, key.Type, items)
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
