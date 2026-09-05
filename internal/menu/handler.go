// Package menu implements the Telegram self-serve commands (/start /link
// /connect /setup /menu /status /undelivered /help), the inline keyboards, and
// the /setup state machine (D-1 ritual). Sender identity always comes from
// update.From.ID, never from callback payloads (anti-forgery).
package menu

import (
	"context"
	"fmt"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/spluft/tgNtfy/internal/catalog"
	"github.com/spluft/tgNtfy/internal/store"
	"github.com/spluft/tgNtfy/internal/tgbot"
	"log/slog"
	"strings"
	"sync"
)

// BotAPI is the subset of the tgbot client the menu needs (mockable).
type BotAPI interface {
	SendMessage(ctx context.Context, chatID any, threadID int, text string) (int, error)
	SendKeyboard(ctx context.Context, chatID any, threadID int, text string, kb [][]models.InlineKeyboardButton) error
	CreateTopic(ctx context.Context, chatID any, name string) (int, error)
	GetChat(ctx context.Context, chatID any) (*tgbot.ChatInfo, error)
	SenderIsGroupAdmin(ctx context.Context, chatID any, userID int64) bool
	BotCanManageTopics(ctx context.Context, chatID any) bool
	AnswerCallback(ctx context.Context, id string, showAlert bool, text string)
}

// Gate is the slice of the store + catalog the menu uses.
type Gate struct {
	Store *store.Store
	Cat   *catalog.Lookup
	Log   *slog.Logger
}

// SetupState holds the in-memory /setup state machine (S0-S3, lost on restart).
type SetupState struct {
	mu       sync.Mutex
	chatUser map[int64]int64 // group chatId -> userID (who is setting it up)
}

// Handler routes TG updates to the command/callback handlers.
type Handler struct {
	api   BotAPI
	gate  *Gate
	setup *SetupState
}

// NewHandler builds the menu handler.
func NewHandler(api BotAPI, st *store.Store, cat *catalog.Lookup, log *slog.Logger) *Handler {
	return &Handler{
		api:   api,
		gate:  &Gate{Store: st, Cat: cat, Log: log},
		setup: &SetupState{chatUser: map[int64]int64{}},
	}
}

// DefaultHandler dispatches a raw update to the appropriate flow.
func (h *Handler) DefaultHandler(ctx context.Context, b *bot.Bot, u *models.Update) {
	if u.Message != nil {
		h.handleMessage(ctx, b, u)
		return
	}
	if u.CallbackQuery != nil {
		h.handleCallback(ctx, b, u.CallbackQuery)
		return
	}
}

func (h *Handler) handleMessage(ctx context.Context, b *bot.Bot, u *models.Update) {
	m := u.Message
	if m == nil || m.From == nil {
		return
	}
	// Record which user was last active in a forum chat (S2 resolution).
	if m.Chat != (models.Chat{}) && m.Chat.IsForum && m.Chat.ID != 0 {
		h.setup.mu.Lock()
		h.setup.chatUser[m.Chat.ID] = m.From.ID
		h.setup.mu.Unlock()
	}

	isPrivate := m.Chat.Type == models.ChatTypePrivate
	txt := strings.TrimSpace(m.Text)

	// Ensure user exists.
	if err := h.gate.Store.UpsertUser(ctx, m.From.ID, m.From.Username, m.From.FirstName); err != nil {
		h.gate.Log.Error("upsert user", "err", err)
	}

	switch {
	case strings.HasPrefix(txt, "/start"):
		h.cmdStart(ctx, m)
	case txt == "/setup" || strings.HasPrefix(txt, "/setup@"):
		h.cmdSetup(ctx, m)
	case isPrivate && (strings.HasPrefix(txt, "/link") || strings.HasPrefix(txt, "/link@")):
		h.cmdLink(ctx, m)
	case strings.HasPrefix(txt, "/connect"):
		h.cmdConnect(ctx, m)
	case txt == "/menu" || strings.HasPrefix(txt, "/menu@"):
		h.cmdMenu(ctx, m)
	case txt == "/status" || strings.HasPrefix(txt, "/status@"):
		h.cmdStatus(ctx, m)
	case txt == "/undelivered" || strings.HasPrefix(txt, "/undelivered@"):
		h.cmdUndelivered(ctx, m)
	case txt == "/help" || strings.HasPrefix(txt, "/help@"):
		h.send(ctx, m.Chat.ID, helpText)
	default:
		h.api.AnswerCallback(ctx, "", false, "")
		_ = h.send(ctx, m.Chat.ID, "Unknown command. Try /menu, /status or /setup.")
	}
}

func (h *Handler) send(ctx context.Context, chatID int64, text string) error {
	_, err := h.api.SendMessage(ctx, chatID, 0, text)
	return err
}

// handleCallback routes inline keyboard callbacks by their data prefix.
func (h *Handler) handleCallback(ctx context.Context, b *bot.Bot, cq *models.CallbackQuery) {
	uid := cq.From.ID
	if cq.Message.Message == nil {
		return
	}
	chatID := cq.Message.Message.Chat.ID
	data := cq.Data
	switch {
	case strings.HasPrefix(data, "setup:"):
		// "setup:done:<uid>" — verify the S2 conditions.
		h.api.AnswerCallback(ctx, cq.ID, false, "")
		h.verifySetup(ctx, uid, chatID)
	case strings.HasPrefix(data, "link:svc:"):
		h.api.AnswerCallback(ctx, cq.ID, false, "")
		svc := strings.TrimPrefix(data, "link:svc:")
		h.issueLinkCode(ctx, uid, chatID, svc)
	case strings.HasPrefix(data, "menu:"):
		h.handleMenuCallback(ctx, uid, chatID, cq)
	case data == "link:list":
		h.api.AnswerCallback(ctx, cq.ID, false, "")
		kb := h.serviceKeyboard(ctx)
		if len(kb) == 0 {
			_ = h.send(ctx, chatID, "No services available yet.")
			return
		}
		h.api.SendKeyboard(ctx, chatID, 0, "Link a service — pick one:", kb)
	case data == "retry_failed":
		h.api.AnswerCallback(ctx, cq.ID, false, "")
		n, err := h.gate.Store.RetryAllFailed(ctx, 50)
		if err != nil {
			_ = h.send(ctx, chatID, "Retry failed.")
			return
		}
		_ = h.send(ctx, chatID, fmt.Sprintf("🔁 Requeued %d.", n))
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func removeStr(list []string, v string) []string {
	out := list[:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func ifStr(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
