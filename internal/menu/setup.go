// The /setup S1->S3 state machine and its verification.
package menu

import (
	"context"
	"errors"
	"fmt"
	"github.com/go-telegram/bot/models"
	"github.com/spluft/tgNtfy/internal/store"
	"strings"
	"time"
)

// cmdSetup implements the /setup state machine (UC-1, §11.3).
func (h *Handler) cmdSetup(ctx context.Context, m *models.Message) {
	uid := m.From.ID
	h.api.SendKeyboard(ctx, m.Chat.ID, 0, setupStep1Text,
		[][]models.InlineKeyboardButton{{{
			Text: "✅ I did it", CallbackData: "setup:done:" + fmt.Sprint(uid),
		}}})
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
	text := fmt.Sprintf("📋 STEP 2/2 — Open your new group and send:\n/connect %s\nYour code: **%s** (10 min). Your linked services will get a topic each.", code, code)
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
	// V-9: a new group means old message_thread_ids point at the previous chat — drop the
	// user's stale group_topics; topics reappear lazily per linked service (link/first event).
	if err := h.gate.Store.ClearUserTopics(ctx, uid); err != nil {
		h.gate.Log.Warn("clear stale topics", "err", err)
	}
	ci, _ := h.api.GetChat(ctx, chatID)
	title := ""
	if ci != nil {
		title = ci.Title
	}
	tp := h.linkedServiceNames(ctx, uid)
	_ = h.send(ctx, chatID, "✅ Setup complete"+(ifStr(title != "", " in "+title, ""))+"."+ifStr(len(tp) > 0, "\nLinked: "+strings.Join(tp, " · "), "")+"\nEvents appear in their topics. Per-service mute: /menu.")
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

func (h *Handler) linkedServiceNames(ctx context.Context, uid int64) []string {
	var out []string
	for _, svc := range h.gate.Store.LinkedServices(ctx, uid) {
		dn, _ := h.gate.Store.DisplayName(ctx, svc)
		out = append(out, dn)
	}
	return out
}
