// Package itest hosts the tgNtfy integration suite: a mock Telegram Bot API HTTP
// server (impersonating api.telegram.org/bot<token>) plus a full pipeline harness
// (store + catalog + ingest + coalesce + transport + tgbot) so the behavioral
// acceptance criteria are verified through the real HTTP path (TG_API_URL -> mock).
package itest

import (
	"context"
	"encoding/json"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
)

// call records one outbound Bot API call.
type call struct {
	Method string
	Form   map[string]any // decoded form fields (chat_id, text, message_thread_id, name, ...)
}

// mockBot is an httptest stand-in for api.telegram.org/bot<token>.
type mockBot struct {
	token string
	mu    sync.Mutex
	calls []call

	forumTitle   string
	isForum      bool
	botAdmTopics bool
	senderAdmin  bool
	nextThread   int
	msgID        int
	srv          *httptest.Server

	// hook lets a test force an error code for a method.
	hook func(method string) (code int, forced bool)
}

func newMockBot(token string) (*mockBot, error) {
	m := &mockBot{
		token:        token,
		forumTitle:   "My tgntfy",
		isForum:      true,
		botAdmTopics: true,
		senderAdmin:  true,
		nextThread:   1000,
		msgID:        7000,
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	return m, nil
}

func (m *mockBot) URL() string { return m.srv.URL }
func (m *mockBot) Close()      { m.srv.Close() }
func (m *mockBot) Addr()       {}

func (m *mockBot) handle(w http.ResponseWriter, r *http.Request) {
	method := botMethod(r)
	form := parseForm(r)

	if m.hook != nil {
		if code, forced := m.hook(method); forced {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":` + itoa(code) + `,"description":"forced"}`))
			return
		}
	}
	m.mu.Lock()
	m.calls = append(m.calls, call{Method: method, Form: form})
	m.mu.Unlock()

	var result any
	switch method {
	case "sendMessage":
		m.msgID++
		result = map[string]any{"message_id": m.msgID}
	case "createForumTopic":
		m.nextThread++
		result = map[string]any{"message_thread_id": m.nextThread}
	case "getChat":
		result = map[string]any{"id": 0, "type": m.chatType(), "title": m.forumTitle, "is_forum": m.isForum}
	case "getChatMember":
		if m.senderAdmin {
			result = map[string]any{"status": "administrator", "user": map[string]any{"id": 123, "is_bot": false}}
		} else {
			result = map[string]any{"status": "member", "user": map[string]any{"id": 123, "is_bot": false}}
		}
	case "getChatAdministrators":
		admins := []any{map[string]any{
			"status": "administrator", "user": map[string]any{"id": 2, "is_bot": true},
			"can_manage_topics": m.botAdmTopics,
		}}
		result = admins
	case "answerCallbackQuery", "setMyCommands", "getUpdates":
		result = true
	default:
		result = true
	}
	writeOK(w, result)
}

func (m *mockBot) chatType() string {
	if m.isForum {
		return "supergroup"
	}
	return "group"
}

func (m *mockBot) sendMessages() []call {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []call
	for _, c := range m.calls {
		if c.Method == "sendMessage" {
			out = append(out, c)
		}
	}
	return out
}

func (m *mockBot) createTopics() []call {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []call
	for _, c := range m.calls {
		if c.Method == "createForumTopic" {
			out = append(out, c)
		}
	}
	return out
}

// botMethod extracts the API method from path "/bot<token>/<method>".
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

// parseForm decodes the multipart/form or urlencoded body go-telegram/bot posts.
func parseForm(r *http.Request) map[string]any {
	out := map[string]any{}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "multipart/form-data" {
		_ = r.ParseMultipartForm(1 << 20)
		for k, vs := range r.MultipartForm.Value {
			if len(vs) > 0 {
				out[k] = vs[0]
			}
		}
		return out
	}
	_ = r.ParseForm()
	for k, vs := range r.PostForm {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}

func writeOK(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

var _ = context.Background
var _ = slog.Default
var _ = os.Getenv
