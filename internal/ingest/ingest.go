// Package ingest implements the HTTP ingestion surface: POST /v1/events (schema, auth,
// size cap, rate limit, idempotency, routing, coalescing) and POST /v1/link.
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/spluft/tgNtfy/internal/catalog"
	"github.com/spluft/tgNtfy/internal/coalesce"
	"github.com/spluft/tgNtfy/internal/limit"
	"github.com/spluft/tgNtfy/internal/store"
	"github.com/spluft/tgNtfy/internal/topic"
)

// maxBodyBytes caps the envelope body (8 KB, E-4).
const maxBodyBytes = 8192

var (
	eventsIn = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "events_in_total", Help: "Accepted events (post auth/schema).",
	}, []string{"service"})
	eventsRejected = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "events_rejected_total", Help: "Rejected events.",
	}, []string{"service", "reason"})
	eventsUnrouted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "events_unrouted_total", Help: "Accepted, no linked users.",
	}, []string{"service"})
	catalogUnknown = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "catalog_unknown_total", Help: "Unknown service/type combos.",
	}, []string{"service", "type"})
	catalogDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "catalog_dropped_total", Help: "Dropped events.",
	}, []string{"service", "type"})
	coalesceBatches = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "coalesce_batches_total", Help: "Coalesce flushes.",
	}, []string{"window"})
	coalesceBatchSize = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "coalesce_batch_size",
		Help:    "Batch size distribution.",
		Buckets: prometheus.DefBuckets,
	})
)

// Envelope is the frozen ingest contract (§3.2).
type Envelope struct {
	V        int            `json:"v"`
	EventID  string         `json:"event_id"`
	Service  string         `json:"service"`
	UserRef  string         `json:"user_ref"`
	Type     string         `json:"type"`
	Severity string         `json:"severity"`
	Title    string         `json:"title"`
	Text     string         `json:"text"`
	URL      string         `json:"url"`
	Metadata map[string]any `json:"metadata"`
}

// Handler is the ingest HTTP handler.
type Handler struct {
	store         *store.Store
	cat           *catalog.Lookup
	limits        *limit.Registry
	coalesc       *coalesce.Coalescer
	log           *slog.Logger
	now           func() time.Time
	topicResolver func(ctx context.Context, userID, chatID int64, service string) (int, error)
}

// Opt is a Handler constructor option.
type Opt func(*Handler)

// NewHandler builds the ingest handler wiring store, catalog and coalescer.
func NewHandler(st *store.Store, cat *catalog.Lookup, log *slog.Logger, opts ...Opt) *Handler {
	if log == nil {
		log = slog.Default()
	}
	h := &Handler{
		store:  st,
		cat:    cat,
		limits: limit.NewRegistry(),
		log:    log,
		now:    time.Now,
		topicResolver: func(ctx context.Context, userID, chatID int64, service string) (int, error) {
			return 0, nil
		},
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

// WithTopicResolver wires the lazy topic creation callback (from tgbot).
func WithTopicResolver(r func(ctx context.Context, userID, chatID int64, service string) (int, error)) Opt {
	return func(h *Handler) { h.topicResolver = r }
}

// WithCoalescer wires a shared coalescer.
func WithCoalescer(c *coalesce.Coalescer) Opt { return func(h *Handler) { h.coalesc = c } }

// SetCoalescer attaches the coalescer (called after construction).
func (h *Handler) SetCoalescer(c *coalesce.Coalescer) { h.coalesc = c }

// Routes returns the mux with ingest endpoints mounted.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/events", h.handleEvents)
	mux.HandleFunc("/v1/link", h.handleLink)
	return mux
}

func writeErr(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "detail": detail})
}

// authService verifies the X-Service-Token and returns the service id.
func (h *Handler) authService(ctx context.Context, w http.ResponseWriter, r *http.Request, requireEnabled bool) (string, bool) {
	tok := r.Header.Get("X-Service-Token")
	if tok == "" {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "")
		eventsRejected.WithLabelValues("_", "unauthorized").Inc()
		return "", false
	}
	svc, ok := h.store.VerifyToken(ctx, tok)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "")
		eventsRejected.WithLabelValues("_", "unauthorized").Inc()
		return "", false
	}
	if requireEnabled {
		if en, _ := h.store.ServiceEnabled(ctx, svc); !en {
			writeErr(w, http.StatusBadRequest, "service_unknown", "")
			eventsRejected.WithLabelValues(svc, "service_unknown").Inc()
			return "", false
		}
	}
	return svc, true
}

func (h *Handler) handleLink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method", "")
		return
	}
	ctx := r.Context()
	svc, ok := h.authService(r.Context(), w, r, true)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "schema", "read body")
		return
	}
	var lr struct {
		Service     string `json:"service"`
		UserRef     string `json:"user_ref"`
		Code        string `json:"code"`
		DisplayName string `json:"display_name"`
	}
	if err := json.Unmarshal(body, &lr); err != nil {
		writeErr(w, http.StatusBadRequest, "schema", "invalid json")
		return
	}
	if lr.Service == "" || lr.UserRef == "" || lr.Code == "" {
		writeErr(w, http.StatusBadRequest, "schema", "service, user_ref and code required")
		return
	}
	if len(lr.DisplayName) > 256 {
		writeErr(w, http.StatusBadRequest, "schema", "display_name must be ≤256 chars")
		return
	}
	if lr.Service != svc {
		writeErr(w, http.StatusBadRequest, "code_mismatch", "service field does not match token service")
		return
	}
	// service must be registered
	if ok, _ := h.store.HasService(ctx, lr.Service); !ok {
		writeErr(w, http.StatusBadRequest, "service_unknown", "")
		return
	}
	// The code row records which TG user the code was issued to. POST /v1/link is
	// called BY the service, so the identity (tg user) is resolved from the code.
	userID, err := h.userIDForLinkCode(ctx, lr.Code)
	if err != nil {
		if errors.Is(err, store.ErrCodeInvalid) {
			writeErr(w, http.StatusBadRequest, "code_invalid", "code not found or expired")
		} else {
			writeErr(w, http.StatusBadRequest, "code_invalid", "")
		}
		return
	}
	// display_name is OPTIONAL (V-2). Sanitize to the canonical topic name; an empty
	// result is treated as absent (the existing stored name is kept, never an error).
	dispName := ""
	if dn := topic.SanitizeTopicName(lr.DisplayName); dn != "" {
		dispName = dn
	}
	if err := h.store.LinkIdentity(ctx, lr.Service, lr.UserRef, lr.Code, userID, userID, dispName); err != nil {
		switch {
		case errors.Is(err, store.ErrAlreadyLinked):
			writeErr(w, http.StatusConflict, "already_linked", "")
		case errors.Is(err, store.ErrCodeUsed):
			writeErr(w, http.StatusBadRequest, "code_invalid", "code already used")
		case errors.Is(err, store.ErrCodeMismatch):
			writeErr(w, http.StatusBadRequest, "code_mismatch", "")
		case errors.Is(err, store.ErrCodeInvalid):
			writeErr(w, http.StatusBadRequest, "code_invalid", "")
		default:
			h.log.Error("link identity failed", "service", lr.Service, "user_ref", lr.UserRef, "err", err)
			writeErr(w, http.StatusInternalServerError, "internal", "")
		}
		return
	}
	h.log.Info("identity linked", "service", lr.Service, "user_ref", lr.UserRef, "user_id", userID)

	// Topic step (V-3): only when the user is ALREADY in group mode with a group chat.
	// topic_created=true iff a group_topics row exists for (user,service) afterwards
	// (created now OR pre-existing). In dm mode no topic is created yet — deferred to
	// first-event delivery (D-FALL). A link that creates no topic is CORRECT.
	topicCreated := false
	if u, gu := h.store.GetUser(ctx, userID); gu == nil && u != nil && u.DeliveryMode == "group" && u.GroupChatID != nil {
		if _, _, terr := h.store.EnsureTopic(ctx, userID, *u.GroupChatID, lr.Service,
			func(chatID int64, svc string) (int, error) { return h.topicResolver(ctx, userID, chatID, svc) }); terr != nil {
			h.log.Warn("lazy topic at link failed; first-event fallback will retry",
				"service", lr.Service, "user_id", userID, "err", terr)
		} else {
			// topic_created=true iff a group_topics row exists afterwards (created NOW or
			// pre-existing on an idempotent re-link, V-4) — §3.3.
			topicCreated = true
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "linked", "user_id": userID, "topic_created": topicCreated})
}

// userIDForLinkCode resolves the TG user who owns a link code.
func (h *Handler) userIDForLinkCode(ctx context.Context, code string) (int64, error) {
	return h.store.UserIDForLinkCode(ctx, code)
}

func (h *Handler) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method", "")
		return
	}
	ctx := r.Context()
	// E-4: size check before JSON.parse.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "schema", "read body")
		return
	}
	if len(body) > maxBodyBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "")
		eventsRejected.WithLabelValues("_", "too_large").Inc()
		return
	}
	svc, ok := h.authService(ctx, w, r, true)
	if !ok {
		return
	}
	var ev Envelope
	if err := json.Unmarshal(body, &ev); err != nil {
		writeErr(w, http.StatusBadRequest, "schema", "invalid json")
		eventsRejected.WithLabelValues(svc, "schema").Inc()
		return
	}
	if err := validateEnvelope(&ev); err != nil {
		writeErr(w, http.StatusBadRequest, "schema", err.Error())
		eventsRejected.WithLabelValues(svc, "schema").Inc()
		return
	}
	if ev.Service != "" && ev.Service != svc {
		writeErr(w, http.StatusBadRequest, "schema", "service field does not match token service")
		eventsRejected.WithLabelValues(svc, "schema").Inc()
		return
	}
	ev.Service = svc

	// E-5 rate limit.
	if !h.limits.AllowForService(svc, h.now()) {
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "")
		eventsRejected.WithLabelValues(svc, "rate_limited").Inc()
		return
	}

	// D-3 / drop-rule: type job_progress (or catalog drop:true) is dropped.
	if et, known := h.cat.Get().TypeFlag(svc, ev.Type); known && et.Drop || (!known && ev.Type == "job_progress") {
		catalogDropped.WithLabelValues(svc, ev.Type).Inc()
		if !known {
			catalogUnknown.WithLabelValues(svc, ev.Type).Inc()
		}
		h.log.Debug("event dropped (drop rule)", "service", svc, "type", ev.Type)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "accepted", "queued": 0})
		return
	}
	if !h.cat.Get().IsKnown(svc, ev.Type) {
		catalogUnknown.WithLabelValues(svc, ev.Type).Inc()
		h.log.Warn("unknown service/type accepted", "service", svc, "type", ev.Type)
	}

	// Idempotency: persist event; duplicate -> 409.
	se := &store.Event{
		EventID:    ev.EventID,
		Service:    svc,
		UserRef:    ev.UserRef,
		Type:       ev.Type,
		Severity:   ev.Severity,
		Title:      ev.Title,
		Text:       ev.Text,
		URL:        ev.URL,
		Metadata:   store.MarshalMetadata(ev.Metadata),
		ReceivedAt: h.now(),
	}
	if err := h.store.InsertEvent(ctx, se); err != nil {
		if errors.Is(err, store.ErrDuplicateEvent) {
			writeErr(w, http.StatusConflict, "duplicate", "")
			eventsRejected.WithLabelValues(svc, "duplicate").Inc()
			return
		}
		h.log.Error("insert event failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal", "")
		return
	}
	eventsIn.WithLabelValues(svc).Inc()

	// Route to targets.
	targets, _ := h.store.ResolveRoutes(ctx, se, h.topicResolver)
	if len(targets) == 0 {
		eventsUnrouted.WithLabelValues(svc).Inc()
	} else {
		for _, t := range targets {
			if h.coalesc != nil {
				h.coalesc.Add(ctx, coalesce.Key{UserID: t.UserID, Service: svc, Type: ev.Type}, &coalesce.Item{
					UserID: t.UserID, EventID: ev.EventID, Severity: ev.Severity,
					Title: ev.Title, Text: ev.Text, URL: ev.URL,
				})
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "accepted", "queued": len(targets)})
}

// validateEnvelope enforces §3.2 field rules.
func validateEnvelope(ev *Envelope) error {
	switch {
	case ev.V != 1:
		return errors.New("v must be 1")
	case ev.EventID == "" || len(ev.EventID) > 255:
		return errors.New("event_id must be 1..255 chars")
	case ev.Service == "" || len(ev.Service) > 32:
		return errors.New("service must be 1..32 chars")
	case ev.Service == "":
		return errors.New("service required")
	case ev.UserRef == "":
		return errors.New("user_ref required")
	case ev.Type == "" || len(ev.Type) > 64:
		return errors.New("type must be 1..64 chars")
	case !validSeverity(ev.Severity):
		return errors.New("severity must be info|warn|error|success")
	case ev.Title == "" || len(ev.Title) > 200:
		return errors.New("title must be 1..200 chars")
	case len(ev.Text) > 500:
		return errors.New("text must be ≤500 chars")
	case len(ev.URL) > 500:
		return errors.New("url must be ≤500 chars")
	}
	return nil
}

func validSeverity(s string) bool {
	switch s {
	case "info", "warn", "error", "success":
		return true
	}
	return false
}

// Render single-event message (§8.5).
func RenderMessage(sev, display, typ, title, text, url string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s — %s\n%s", iconFor(sev), display, typ, title)
	if text != "" {
		b.WriteString("\n" + truncate(text, 120))
	}
	if url != "" {
		b.WriteString("\n" + url)
	}
	return truncate(b.String(), 3500)
}

// RenderBatch renders a coalesced batch (§8.5).
func RenderBatch(sev, display, typ string, items []*coalesce.Item) string {
	var b strings.Builder
	fmt.Fprintf(&b, "⚡ %d × %s %s: %s", len(items), iconFor(sev), display, typ)
	shared := sharedURL(items)
	for _, it := range items {
		b.WriteString("\n• " + truncate(it.Title, 200))
		if it.Text != "" {
			b.WriteString(" — " + truncate(it.Text, 120))
		}
	}
	if shared != "" {
		b.WriteString("\n" + shared)
	}
	return truncate(b.String(), 3500)
}

func sharedURL(items []*coalesce.Item) string {
	if len(items) < 2 {
		return ""
	}
	first := items[0].URL
	for _, it := range items[1:] {
		if it.URL != first || it.URL == "" {
			return ""
		}
	}
	return first
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-12] + "… [truncated]"
}

// RecordBatch records a coalesce flush batch size (metric).
func RecordBatch(n int) {
	coalesceBatches.WithLabelValues("flush").Inc()
	coalesceBatchSize.Observe(float64(n))
}

func iconFor(sev string) string {
	switch sev {
	case "success":
		return "✅"
	case "error":
		return "❌"
	case "warn":
		return "⚠️"
	default:
		return "ℹ️"
	}
}
