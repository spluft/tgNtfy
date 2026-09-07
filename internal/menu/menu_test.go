package menu

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"
	"github.com/spluft/tgNtfy/internal/catalog"
	"github.com/spluft/tgNtfy/internal/store"
	"github.com/spluft/tgNtfy/internal/tgbot"
)

// fakeAPI records outbound menu calls so tests can assert routing/behaviour without the
// Telegram network. It implements menu.BotAPI.
type fakeAPI struct {
	sent   []string                // sendMessage texts
	kbs    [][]models.InlineKeyboardButton // last keyboard sent
	sentKB []string
	topics []int
	chatInfo *tgbot.ChatInfo
	chatErr  error
	senderAdmin    bool
	botAdminTopics bool
	answered []string
}

func (f *fakeAPI) SendMessage(ctx context.Context, chatID any, threadID int, text string) (int, error) {
	f.sent = append(f.sent, text)
	return 1, nil
}
func (f *fakeAPI) SendKeyboard(ctx context.Context, chatID any, threadID int, text string, kb [][]models.InlineKeyboardButton) error {
	f.sentKB = append(f.sentKB, text)
	f.kbs = kb
	return nil
}
func (f *fakeAPI) CreateTopic(ctx context.Context, chatID any, name string) (int, error) {
	f.topics = append(f.topics, 1)
	return 1, nil
}
func (f *fakeAPI) GetChat(_ context.Context, _ any) (*tgbot.ChatInfo, error) {
	if f.chatErr != nil {
		return nil, f.chatErr
	}
	if f.chatInfo != nil {
		return f.chatInfo, nil
	}
	return &tgbot.ChatInfo{IsForum: true, Title: "My Group"}, nil
}
func (f *fakeAPI) SenderIsGroupAdmin(ctx context.Context, chatID any, userID int64) bool { return f.senderAdmin || true }
func (f *fakeAPI) BotCanManageTopics(ctx context.Context, chatID any) bool               { return f.botAdminTopics || true }
func (f *fakeAPI) AnswerCallback(ctx context.Context, id string, showAlert bool, text string) {
	f.answered = append(f.answered, id)
}

func testGate(t *testing.T) (*Handler, *fakeAPI) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	st.UpsertUser(ctx, 1000, "alice", "Alice")
	st.CreateService(ctx, "govpn", "VPN", "tok")
	st.SetEnabled(ctx, "govpn", true)
	st.EnsureServiceUser(ctx, "govpn", "1", 1000)
	cat, _ := catalog.Load(filepath.Join("..", "..", "config", "events.yaml"))
	api := &fakeAPI{}
	h := NewHandler(api, st, catalog.NewLookup(cat), slog.Default())
	return h, api
}

func userMsg(from int64, text string, chatType models.ChatType) *models.Update {
	return &models.Update{Message: &models.Message{
		ID: 1, From: &models.User{ID: from, FirstName: "U"},
		Chat: models.Chat{ID: 2000, Type: chatType}, Text: text,
	}}
}

func TestCmdStartLinkHelpStatus(t *testing.T) {
	h, api := testGate(t)
	ctx := context.Background()
	h.DefaultHandler(ctx, nil, userMsg(1000, "/start", "private"))
	if len(api.sent) == 0 || !strings.Contains(api.sent[0], "Hi!") {
		t.Fatalf("/start got %q", api.sent)
	}
	// /link shows the service keyboard
	h.DefaultHandler(ctx, nil, userMsg(1000, "/link", "private"))
	if len(api.sentKB) == 0 || !strings.Contains(api.sentKB[0], "pick one") {
		t.Fatalf("/link got %q", api.sentKB)
	}
	// /status
	h.DefaultHandler(ctx, nil, userMsg(1000, "/status", "private"))
	if len(api.sent) < 2 || !strings.Contains(api.sent[len(api.sent)-1], "Mode") {
		t.Fatalf("/status got %q", api.sent)
	}
	// /help
	h.DefaultHandler(ctx, nil, userMsg(1000, "/help", "private"))
	if !strings.Contains(api.sent[len(api.sent)-1], "Commands:") {
		t.Fatalf("/help got %q", api.sent)
	}
	// unknown command
	h.DefaultHandler(ctx, nil, userMsg(1000, "/bogus", "private"))
	if !strings.Contains(api.sent[len(api.sent)-1], "Unknown command") {
		t.Fatalf("unknown got %q", api.sent)
	}
}

func TestCmdConnectOutsideGroup(t *testing.T) {
	h, api := testGate(t)
	ctx := context.Background()
	h.DefaultHandler(ctx, nil, userMsg(1000, "/connect 123456", "private"))
	if len(api.sent) == 0 || !strings.Contains(api.sent[0], "inside your private group") {
		t.Fatalf("dm /connect got %q", api.sent)
	}
}

func TestSetupVerifyFlow(t *testing.T) {
	h, api := testGate(t)
	ctx := context.Background()
	// /setup issues step-1 keyboard
	h.DefaultHandler(ctx, nil, userMsg(1000, "/setup", "private"))
	if len(api.sentKB) == 0 || !strings.Contains(api.sentKB[0], "STEP 1/2") {
		t.Fatalf("/setup got %q", api.sentKB)
	}
	// callback setup:done:<uid> triggers verifySetup against a group chat
	cq := &models.CallbackQuery{
		ID: "cb1", From: models.User{ID: 1000, FirstName: "U"},
		Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: 1, Chat: models.Chat{ID: 5000, Type: "supergroup"}}},
		Data:    "setup:done:1000",
	}
	u := &models.Update{CallbackQuery: cq}
	h.DefaultHandler(ctx, nil, u)
	// verifySetup should issue a connect code text
	found := false
	for _, s := range api.sent {
		if strings.Contains(s, "STEP 2/2") && strings.Contains(s, "/connect ") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected STEP 2/2 connect-code text, got %q", api.sent)
	}
}

func TestMenuLevelsAndToggles(t *testing.T) {
	h, api := testGate(t)
	ctx := context.Background()
	// /menu -> level-1 with govpn + link-another
	h.DefaultHandler(ctx, nil, userMsg(1000, "/menu", "private"))
	if len(api.kbs) == 0 {
		t.Fatal("expected level-1 keyboard")
	}
	// callback menu:svc:govpn -> level-2 with event types
	h.DefaultHandler(ctx, nil, cbQuery(1000, 2000, "menu:svc:govpn"))
	if len(api.kbs) == 0 || len(api.kbs) < 2 {
		t.Fatalf("level-2 rows: %+v", api.kbs)
	}
	// mute toggle
	h.DefaultHandler(ctx, nil, cbQuery(1000, 2000, "menu:mute:govpn"))
	// type toggle
	h.DefaultHandler(ctx, nil, cbQuery(1000, 2000, "menu:type:govpn:vpn_connected"))
	// back
	h.DefaultHandler(ctx, nil, cbQuery(1000, 2000, "menu:back:"))
	// retry_failed
	h.DefaultHandler(ctx, nil, cbQuery(1000, 2000, "retry_failed"))
}

func cbQuery(from, chatID int64, data string) *models.Update {
	return &models.Update{CallbackQuery: &models.CallbackQuery{
		ID: "cb", From: models.User{ID: from, FirstName: "U"},
		Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: 1, Chat: models.Chat{ID: chatID, Type: "private"}}},
		Data:    data,
	}}
}

func TestCmdUndeliveredNoFails(t *testing.T) {
	h, api := testGate(t)
	ctx := context.Background()
	h.DefaultHandler(ctx, nil, userMsg(1000, "/undelivered", "private"))
	if len(api.sent) == 0 || !strings.Contains(api.sent[0], "Nothing delivered") {
		t.Fatalf("/undelivered got %q", api.sent)
	}
}
