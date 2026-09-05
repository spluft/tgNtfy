package tgbot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func botMethod(r *http.Request) string {
	p := r.URL.Path
	for len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

func fakeBotAPI(t *testing.T, handler func(method string) (int, any)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := botMethod(r)
		var status int
		var result any
		if handler != nil {
			status, result = handler(method)
		}
		w.Header().Set("Content-Type", "application/json")
		if status != 0 && status != 200 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status})
			return
		}
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New("123:test", srv.URL, func(context.Context, *bot.Bot, *models.Update) {})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSendMessageAndCreateTopic(t *testing.T) {
	srv := fakeBotAPI(t, func(method string) (int, any) {
		switch method {
		case "sendMessage":
			return 200, map[string]any{"message_id": 42}
		case "createForumTopic":
			return 200, map[string]any{"message_thread_id": 1001}
		}
		return 200, true
	})
	c := newTestClient(t, srv)
	ctx := context.Background()
	id, err := c.SendMessage(ctx, int64(5), 1001, "hi")
	if err != nil || id != 42 {
		t.Fatalf("SendMessage: id=%d err=%v", id, err)
	}
	thread, err := c.CreateTopic(ctx, int64(5), "VPN")
	if err != nil || thread != 1001 {
		t.Fatalf("CreateTopic: thread=%d err=%v", thread, err)
	}
	_ = c.SendKeyboard(ctx, int64(5), 0, "pick", [][]models.InlineKeyboardButton{{{
		Text: "X", CallbackData: "m:1",
	}}})
	c.AnswerCallback(ctx, "cb", false, "")
	c.SetMyCommands(ctx)
}

func TestSendMessageError(t *testing.T) {
	srv := fakeBotAPI(t, func(method string) (int, any) {
		if method == "sendMessage" {
			return 400, nil
		}
		return 200, true
	})
	c := newTestClient(t, srv)
	if _, err := c.SendMessage(context.Background(), int64(5), 0, "x"); err == nil {
		t.Fatal("expected send error on 400")
	}
}

func TestGetChatAndAdminChecks(t *testing.T) {
	srv := fakeBotAPI(t, func(method string) (int, any) {
		switch method {
		case "getChat":
			return 200, map[string]any{"id": 5, "title": "My G", "is_forum": true}
		case "getChatMember":
			return 200, map[string]any{"status": "administrator", "user": map[string]any{"id": 3, "is_bot": false}}
		case "getChatAdministrators":
			return 200, []any{map[string]any{
				"status": "administrator", "user": map[string]any{"id": 2, "is_bot": true},
				"can_manage_topics": true,
			}}
		}
		return 200, true
	})
	c := newTestClient(t, srv)
	ctx := context.Background()
	ci, err := c.GetChat(ctx, int64(5))
	if err != nil || !ci.IsForum || ci.Title != "My G" {
		t.Fatalf("GetChat: %+v err=%v", ci, err)
	}
	if !c.SenderIsGroupAdmin(ctx, int64(5), 3) {
		t.Fatal("sender should be admin")
	}
	if !c.BotCanManageTopics(ctx, int64(5)) {
		t.Fatal("bot should manage topics")
	}

	srv2 := fakeBotAPI(t, func(method string) (int, any) {
		if method == "getChatMember" {
			return 200, map[string]any{"status": "member", "user": map[string]any{"id": 3, "is_bot": false}}
		}
		if method == "getChat" {
			return 200, map[string]any{"id": 5, "is_forum": true, "title": "g"}
		}
		return 200, true
	})
	c2 := newTestClient(t, srv2)
	if c2.SenderIsGroupAdmin(ctx, int64(5), 3) {
		t.Fatal("member must not be admin")
	}
	ci2, err := c2.GetChat(ctx, int64(5))
	if err != nil || !ci2.IsForum {
		t.Fatalf("GetChat2: %+v err=%v", ci2, err)
	}
}

func TestGetChatError(t *testing.T) {
	srv := fakeBotAPI(t, func(method string) (int, any) {
		if method == "getChat" {
			return 403, nil
		}
		return 200, true
	})
	c := newTestClient(t, srv)
	if _, err := c.GetChat(context.Background(), int64(9)); err == nil {
		t.Fatal("expected getChat error")
	}
	if c.SenderIsGroupAdmin(context.Background(), int64(9), 9) {
		t.Fatal("admin check must be false on error")
	}
}
