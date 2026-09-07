// Self-serve chat commands.
package menu

import (
	"context"
	"fmt"
	"github.com/go-telegram/bot/models"
	"strings"
)

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

// serviceKeyboard builds the link keyboard from ENABLED services in the store (V-10,
// D-MENU-1): a service registers purely via /setup + POST /v1/link — no catalog.
func (h *Handler) serviceKeyboard(ctx context.Context) [][]models.InlineKeyboardButton {
	svcs, err := h.gate.Store.ServiceList(ctx)
	if err != nil {
		h.gate.Log.Error("service list", "err", err)
		return nil
	}
	var kb [][]models.InlineKeyboardButton
	for _, s := range svcs {
		if s.Enabled != 1 {
			continue
		}
		kb = append(kb, []models.InlineKeyboardButton{{
			Text: s.DisplayName, CallbackData: "link:svc:" + s.Service,
		}})
	}
	return kb
}

// cmdLink implements /link (UC-2): pick a linked/available service, then issue a code.
func (h *Handler) cmdLink(ctx context.Context, m *models.Message) {
	kb := h.serviceKeyboard(ctx)
	if len(kb) == 0 {
		_ = h.send(ctx, m.Chat.ID, "No services available yet. Ask the operator to enable a service, then try again.")
		return
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
