// /menu level-1/level-2 keyboards and callback routing.
package menu

import (
	"context"
	"github.com/go-telegram/bot/models"
	"strings"
)

// cmdMenu shows the level-1 service keyboard (UC-4).
func (h *Handler) cmdMenu(ctx context.Context, m *models.Message) {
	h.menuLevel1(ctx, m.From.ID, m.Chat.ID)
}

func (h *Handler) menuLevel1(ctx context.Context, uid, chatID int64) {
	var rows [][]models.InlineKeyboardButton
	for _, svc := range h.gate.Store.LinkedServices(ctx, uid) {
		mut, _ := h.gate.Store.SubscriptionMuted(ctx, uid, svc)
		disp, _ := h.gate.Store.DisplayName(ctx, svc)
		icon := "✅ "
		if mut {
			icon = "🔕 "
		}
		rows = append(rows, []models.InlineKeyboardButton{{
			Text: icon + disp, CallbackData: "menu:svc:" + svc,
		}})
	}
	// Brand-new service: link it via the store-backed keyboard (no catalog).
	rows = append(rows, []models.InlineKeyboardButton{{
		Text: "➕ link another service…", CallbackData: "link:list",
	}})
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
	types := cat.ServiceTypes(svc)
	var rows [][]models.InlineKeyboardButton
	for _, typ := range types {
		icon := "⬜ "
		if contains(enabled, typ) {
			icon = "✅ "
		}
		rows = append(rows, []models.InlineKeyboardButton{{
			Text: icon + typ, CallbackData: "menu:type:" + svc + ":" + typ,
		}})
	}
	muteTxt := "🔕 Mute all"
	if mut {
		muteTxt = "🔔 Unmute"
	}
	rows = append(rows,
		[]models.InlineKeyboardButton{{Text: muteTxt, CallbackData: "menu:mute:" + svc}},
		[]models.InlineKeyboardButton{{Text: "⬅️ Back", CallbackData: "menu:back:"}},
	)
	disp, _ := h.gate.Store.DisplayName(ctx, svc)
	head := disp + " — choose event types:"
	if len(types) == 0 {
		head = disp + " — (event types unknown to the gate — all types on)"
	}
	h.api.SendKeyboard(ctx, chatID, 0, head, rows)
}
