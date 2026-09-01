// Package menu implements the Telegram self-serve commands (/start /link /connect
// /setup /menu /status /undelivered /help), the inline keyboards, and the /setup
// state machine (D-1 ritual). Sender identity always comes from update.From.ID, never
// from callback payloads (anti-forgery).
package menu

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/spluft/tgNtfy/internal/catalog"
	"github.com/spluft/tgNtfy/internal/store"
	"github.com/spluft/tgNtfy/internal/tgbot"
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
	mu        sync.Mutex
	userState map[int64]string // userID -> state label ("s1")
	chatUser  map[int64]int64  // group chatId -> userID (who is setting it up)
}

// Handler routes TG updates to the command/callback handlers.
type Handler struct {
	api   BotAPI
	gate  *Gate
	setup *SetupState
	now   func() time.Time
}

// NewHandler builds the menu handler.
func NewHandler(api BotAPI, st *store.Store, cat *catalog.Lookup, log *slog.Logger) *Handler {
	return &Handler{
		api:   api,
		gate:  &Gate{Store: st, Cat: cat, Log: log},
		setup: &SetupState{userState: map[int64]string{}, chatUser: map[int64]int64{}},
		now:   time.Now,
	}
}

// setNow overrides the clock (tests).
func (h *Handler) setNow(f func() time.Time) { h.now = f }

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

// cmdStart implements /start (UC-2 2a auto-link for govpn first user).
func (h *Handler) cmdStart(ctx context.Context, m *models.Message) {
	uid := m.From.ID
	if err := h.gate.Store.ClaimGovpnAdmin(ctx, uid); err != nil {
		h.gate.Log.Warn("govpn auto-link", "err", err)
	}
	u, _ := h.gate.Store.GetUser(ctx, uid)
	msg := welcomeText
	if u != nil && u.DeliveryMode == "dm" {
		msg += "\n\n(Events go to this DM until you finish /setup.)"
	}
	_ = h.send(ctx, m.Chat.ID, msg)
}

// cmdSetup implements the /setup state machine (UC-1, §11.3).
func (h *Handler) cmdSetup(ctx context.Context, m *models.Message) {
	uid := m.From.ID
	h.setup.mu.Lock()
	h.setup.userState[uid] = "s1"
	h.setup.mu.Unlock()
	h.api.SendKeyboard(ctx, m.Chat.ID, 0, setupStep1Text,
		[][]models.InlineKeyboardButton{{{
			Text: "✅ I did it", CallbackData: "setup:done:" + fmt.Sprint(uid),
		}}})
}

// cmdLink implements /link (UC-2): pick a service from keyboard, then issue a code.
func (h *Handler) cmdLink(ctx context.Context, m *models.Message) {
	cat := h.gate.Cat.Get()
	var kb [][]models.InlineKeyboardButton
	for svc, s := range cat.Services {
		kb = append(kb, []models.InlineKeyboardButton{{
			Text: s.DisplayName, CallbackData: "link:svc:" + svc,
		}})
	}
	h.api.SendKeyboard(ctx, m.Chat.ID, 0, "Link a service — pick one:", kb)
}

// cmdConnect completes the group binding (UC-1 step 3-5).
func (h *Handler) cmdConnect(ctx context.Context, m *models.Message) {
	uid := m.From.ID
	// Parse the code from "/connect 123456" or "/connect@bot 123456".
	parts := strings.Fields(m.Text)
	if len(parts) < 2 {
		_ = h.send(ctx, m.Chat.ID, "Usage: send `/connect <code>` (the 6-digit code from /setup).")
		return
	}
	code := parts[len(parts)-1]
	chatID := m.Chat.ID

	// Must be in a group/supergroup.
	if m.Chat.Type != models.ChatTypeGroup && m.Chat.Type != models.ChatTypeSupergroup {
		_ = h.send(ctx, m.Chat.ID, "Send /connect inside your private group, not here.")
		return
	}
	h.finishSetup(ctx, uid, chatID, code)
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

// verifySetup runs the S2 checks (getChat forum+admin+can_manage_topics+sender-admin).
func (h *Handler) verifySetup(ctx context.Context, uid, chatID int64) {
	// Resolve which group: use the chat the callback came from, or the last forum chat.
	target := chatID
	h.setup.mu.Lock()
	if target == 0 {
		target = h.forumChatOf(uid)
	}
	h.setup.mu.Unlock()
	if target == 0 {
		_ = h.send(ctx, chatID, setupErrNoGroup)
		return
	}

	ci, err := h.api.GetChat(ctx, target)
	if err != nil {
		_ = h.send(ctx, chatID, setupErrNoGroup)
		return
	}
	if !ci.IsForum {
		_ = h.send(ctx, chatID, setupErrNoForum)
		return
	}
	if !h.api.BotCanManageTopics(ctx, target) {
		_ = h.send(ctx, chatID, setupErrNoAdminRight)
		return
	}
	if !h.api.SenderIsGroupAdmin(ctx, target, uid) {
		_ = h.send(ctx, chatID, setupErrSenderNotAdmin)
		return
	}
	// Completed S1/setup verify. Issue a connect code -> S3.
	code := h.newCode()
	if err := h.gate.Store.CreateConnectCode(ctx, code, uid, 10*time.Minute); err != nil {
		h.gate.Log.Error("create connect code", "err", err)
		_ = h.send(ctx, chatID, "Internal error — try again.")
		return
	}
	text := fmt.Sprintf("📋 STEP 2/2 — Open your new group and send:\n/connect %s\nYour code: **%s** (10 min). I'll create one topic per linked service.", code, code)
	_ = h.send(ctx, chatID, text)
}

// finishSetup validates + consumes the connect code and creates topics (UC-1 step 5).
func (h *Handler) finishSetup(ctx context.Context, uid, chatID int64, code string) {
	if err := h.gate.Store.ConsumeConnectCode(ctx, code, uid); err != nil {
		if errors.Is(err, store.ErrCodeUsed) {
			_ = h.send(ctx, chatID, "That code was already used.")
		} else {
			_ = h.send(ctx, chatID, "Code not found or expired. Run /setup in the DM again.")
		}
		return
	}
	gc := chatID
	if err := h.gate.Store.SetDeliveryMode(ctx, uid, "group", &gc); err != nil {
		_ = h.send(ctx, chatID, "Setup failed to save.")
		return
	}
	h.createTopicsFor(ctx, uid, chatID)
	ci, _ := h.api.GetChat(ctx, chatID)
	title := ""
	if ci != nil {
		title = ci.Title
	}
	tp := h.linkedServiceNames(ctx, uid)
	_ = h.send(ctx, chatID, "✅ Setup complete"+(ifStr(title != "", " in "+title, ""))+"."+ifStr(len(tp) > 0, "\nTopics: "+strings.Join(tp, " · "), "")+"\nEvents appear in their topics. Per-service mute: /menu.")
}

// forumChatOf returns a forum chat where uid has been active.
func (h *Handler) forumChatOf(uid int64) int64 {
	for cid, u := range h.setup.chatUser {
		if u == uid {
			return cid
		}
	}
	return 0
}

func (h *Handler) createTopicsFor(ctx context.Context, uid, chatID int64) {
	cat := h.gate.Cat.Get()
	for svc, s := range cat.Services {
		tabular, err := h.api.CreateTopic(ctx, chatID, s.DisplayName)
		if err != nil {
			h.gate.Log.Warn("create topic", "service", svc, "err", err)
			continue
		}
		_ = h.gate.Store.SetTopic(ctx, uid, svc, tabular)
	}
}

func (h *Handler) linkedServiceNames(ctx context.Context, uid int64) []string {
	cat := h.gate.Cat.Get()
	var out []string
	users := h.gate.Store.LinkedServices(ctx, uid)
	for _, svc := range users {
		if s, ok := cat.Services[svc]; ok {
			out = append(out, s.DisplayName)
		} else {
			out = append(out, svc)
		}
	}
	return out
}

// cmdMenu shows the level-1 service keyboard (UC-4).
func (h *Handler) cmdMenu(ctx context.Context, m *models.Message) {
	h.menuLevel1(ctx, m.From.ID, m.Chat.ID)
}

func (h *Handler) menuLevel1(ctx context.Context, uid, chatID int64) {
	cat := h.gate.Cat.Get()
	var rows [][]models.InlineKeyboardButton
	linked := map[string]bool{}
	for _, svc := range h.gate.Store.LinkedServices(ctx, uid) {
		// subscription muted?
		mut, _ := h.gate.Store.SubscriptionMuted(ctx, uid, svc)
		disp := svc
		if s, ok := cat.Services[svc]; ok {
			disp = s.DisplayName
		}
		icon := "✅ "
		if mut {
			icon = "🔕 "
		}
		rows = append(rows, []models.InlineKeyboardButton{{
			Text: icon + disp, CallbackData: "menu:svc:" + svc,
		}})
		linked[svc] = true
	}
	// Unlinked catalog services show a link CTA.
	for svc, s := range cat.Services {
		if !linked[svc] {
			rows = append(rows, []models.InlineKeyboardButton{{
				Text: "➕ link " + s.DisplayName + "…", CallbackData: "link:svc:" + svc,
			}})
		}
	}
	h.api.SendKeyboard(ctx, chatID, 0, "📡 Your services — tap to manage:", rows)
}

// handleMenuCallback handles menu level-2 type toggles, mute-all, back.
func (h *Handler) handleMenuCallback(ctx context.Context, uid, chatID int64, cq *models.CallbackQuery) {
	rest := strings.TrimPrefix(cq.Data, "menu:")
	parts := strings.Split(rest, ":")
	if len(parts) < 2 {
		h.api.AnswerCallback(ctx, cq.ID, false, "")
		return
	}
	action, svc := parts[0], parts[1]
	switch action {
	case "svc":
		h.api.AnswerCallback(ctx, cq.ID, false, "")
		h.menuLevel2(ctx, uid, chatID, svc)
	case "back":
		h.api.AnswerCallback(ctx, cq.ID, false, "")
		h.menuLevel1(ctx, uid, chatID)
	case "mute":
		h.api.AnswerCallback(ctx, cq.ID, false, "Muted "+svc)
		_ = h.gate.Store.SetMuted(ctx, uid, svc, true)
		h.menuLevel2(ctx, uid, chatID, svc)
	case "type":
		if len(parts) < 3 {
			return
		}
		typ := parts[2]
		h.api.AnswerCallback(ctx, cq.ID, false, "")
		cur, _ := h.gate.Store.EventTypesEnabled(ctx, uid, svc)
		if contains(cur, typ) {
			cur = removeStr(cur, typ)
		} else {
			cur = append(cur, typ)
		}
		_ = h.gate.Store.SetEventTypes(ctx, uid, svc, cur)
		h.menuLevel2(ctx, uid, chatID, svc)
	}
}

func (h *Handler) menuLevel2(ctx context.Context, uid, chatID int64, svc string) {
	cat := h.gate.Cat.Get()
	mut, _ := h.gate.Store.SubscriptionMuted(ctx, uid, svc)
	enabled, _ := h.gate.Store.EventTypesEnabled(ctx, uid, svc)
	var rows [][]models.InlineKeyboardButton
	if s, ok := cat.Services[svc]; ok {
		for typ := range s.Events {
			icon := "⬜ "
			if contains(enabled, typ) {
				icon = "✅ "
			}
			rows = append(rows, []models.InlineKeyboardButton{{
				Text: icon + typ, CallbackData: "menu:type:" + svc + ":" + typ,
			}})
		}
	}
	muteTxt := "🔕 Mute all"
	if mut {
		muteTxt = "🔔 Unmute"
	}
	rows = append(rows,
		[]models.InlineKeyboardButton{{Text: muteTxt, CallbackData: "menu:mute:" + svc}},
		[]models.InlineKeyboardButton{{Text: "⬅️ Back", CallbackData: "menu:back:"}},
	)
	disp := svc
	if s, ok := cat.Services[svc]; ok {
		disp = s.DisplayName
	}
	h.api.SendKeyboard(ctx, chatID, 0, disp+" — choose event types:", rows)
}

// issueLinkCode issues a 6-digit link code for a service (UC-2 step 2).
func (h *Handler) issueLinkCode(ctx context.Context, uid, chatID int64, svc string) {
	cat := h.gate.Cat.Get()
	disp := svc
	if s, ok := cat.Services[svc]; ok {
		disp = s.DisplayName
	}
	code := h.newCode()
	if err := h.gate.Store.CreateLinkCode(ctx, code, svc, uid, 10*time.Minute, nil); err != nil {
		_ = h.send(ctx, chatID, "Failed to create a code — try again.")
		return
	}
	_ = h.send(ctx, chatID, fmt.Sprintf("Enter this code in **%s** (Profile → Notifications):\n**%s**\n(expires in 10 min; single use)", disp, code))
}

// cmdStatus implements /status (UC-5).
func (h *Handler) cmdStatus(ctx context.Context, m *models.Message) {
	uid := m.From.ID
	u, _ := h.gate.Store.GetUser(ctx, uid)
	var b strings.Builder
	mode := "dm"
	if u != nil {
		mode = u.DeliveryMode
	}
	fmt.Fprintf(&b, "📊 Mode: **%s**", mode)
	if u != nil && u.GroupChatID != nil {
		b.WriteString(" (group)")
	}
	b.WriteString("\n\n")
	for _, svc := range h.gate.Store.LinkedServices(ctx, uid) {
		last, lt, err := h.gate.Store.LastEventForUserService(ctx, uid, svc)
		if err == nil && last != "" {
			fmt.Fprintf(&b, "%s: last — %s · %s\n", svc, last, lt.Format("15:04"))
		}
	}
	n, _ := h.gate.Store.CountFailed(ctx, uid)
	fmt.Fprintf(&b, "\nUndelivered: %d", n)
	if n > 0 {
		b.WriteString("\n/undelivered to see and retry.")
	}
	_ = h.send(ctx, m.Chat.ID, b.String())
}

// cmdUndelivered implements /undelivered (UC-5).
func (h *Handler) cmdUndelivered(ctx context.Context, m *models.Message) {
	uid := m.From.ID
	fails, _ := h.gate.Store.FailedDeliveries(ctx, 20)
	if len(fails) == 0 {
		_ = h.send(ctx, m.Chat.ID, "Nothing delivered yet.")
		return
	}
	var b strings.Builder
	b.WriteString("❌ Failed deliveries:\n")
	for _, d := range fails {
		fmt.Fprintf(&b, "#%d %s %s — %s (%d/5)\n", d.ID, d.Service, d.Type, firstLine(d.LastErr), d.Attempts)
	}
	h.api.SendKeyboard(ctx, m.Chat.ID, 0, b.String(), [][]models.InlineKeyboardButton{{{
		Text: "🔁 Retry all failed", CallbackData: "retry_failed",
	}}})
	_ = uid
}

// newCode generates a 6-digit code via crypto/rand with rejection sampling
// (avoiding modulo bias toward the top of the byte range).
func (h *Handler) newCode() string {
	const digits = "0123456789"
	for {
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		var sb strings.Builder
		acceptable := true
		for _, c := range b {
			// Rejection sampling: 0..249 give uniform digit classes; 250..255 rejected,
			// forcing a full redraw so all 10 digits are equally likely.
			if c >= 250 {
				acceptable = false
				break
			}
			sb.WriteByte(digits[int(c)%10])
		}
		if acceptable {
			return sb.String()
		}
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

const (
	welcomeText            = "Hi! I'm the tgNtfy gate. Your events from goYouTube, Mail, Recomendarr and VPN will arrive in YOUR personal Telegram forum group — one topic per service.\n1) Link a service: /link\n2) Create your group: /setup\nManage anything in /menu."
	helpText               = "Commands:\n/link — link a service\n/setup — create your forum group\n/connect <code> — bind this group (send in your group)\n/menu — manage services & types\n/status — delivery status\n/undelivered — failed deliveries"
	setupStep1Text         = "📋 STEP 1/2 — Create a new **private** group in Telegram (any name, e.g. 'my tgntfy'). Members: only you. Then add **me** as **Administrator** with the permission **Manage topics** (group → Administrators → Edit → Manage topics ✓).\n\nWhen done, tap ✅ I did it."
	setupErrNoGroup        = "I can't find your group yet. Open your private group and send any message there (e.g. /setup), then tap ✅ I did it again."
	setupErrNoForum        = "This group doesn't have **Topics** enabled — create a forum-style group (group settings → Topics → on) or a new one."
	setupErrNoAdminRight   = "I can see the group, but I'm missing the **Manage topics** admin right. Grant it (group → Administrators → Edit), then tap ✅ I did it again."
	setupErrSenderNotAdmin = "Only an **admin of the group** can finish setup."
)
