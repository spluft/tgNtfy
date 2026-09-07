package tgbot

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// ---- SOCKS5 proxy support (TG_BOT_SOCKS5_PROXY_URL) --------------------------------

// stubSOCKS5 runs a minimal no-auth SOCKS5 server that proxies CONNECT traffic to the
// real target. It counts handled CONNECTs so tests can assert traffic went through it.
func stubSOCKS5(t *testing.T) (url string, connects *int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var n int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				meth := make([]byte, 2) // VER, NMETHODS
				if _, err := io.ReadFull(c, meth); err != nil {
					return
				}
				if meth[1] > 0 { // consume advertised methods
					b := make([]byte, meth[1])
					if _, err := io.ReadFull(c, b); err != nil {
						return
					}
				}
				if _, err := c.Write([]byte{0x05, 0x00}); err != nil { // no auth
					return
				}
				hdr := make([]byte, 4)
				if _, err := io.ReadFull(c, hdr); err != nil {
					return
				}
				if hdr[0] != 0x05 || hdr[1] != 0x01 { // CONNECT only
					return
				}
				var host string
				switch hdr[3] {
				case 0x01: // IPv4
					b := make([]byte, 4)
					if _, err := io.ReadFull(c, b); err != nil {
						return
					}
					host = net.IP(b).String()
				case 0x03: // domain
					lb := make([]byte, 1)
					if _, err := io.ReadFull(c, lb); err != nil {
						return
					}
					b := make([]byte, lb[0])
					if _, err := io.ReadFull(c, b); err != nil {
						return
					}
					host = string(b)
				default:
					return
				}
				pb := make([]byte, 2)
				if _, err := io.ReadFull(c, pb); err != nil {
					return
				}
				port := binary.BigEndian.Uint16(pb)
				remote, err := net.Dial("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
				if err != nil {
					_, _ = c.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
					return
				}
				defer remote.Close()
				_, _ = c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
				n++
				done := make(chan struct{})
				go func() { _, _ = io.Copy(remote, c); close(done) }()
				_, _ = io.Copy(c, remote)
				<-done
			}(c)
		}
	}()
	return "socks5://" + ln.Addr().String(), &n
}

// (a) empty proxy URL -> default path (direct), mock Bot API via TG_API_URL still green.
// (b) valid socks5 URL -> traffic flows through the proxy (CONNECT count > 0).
// (c) malformed URL -> clean error, no panic.
func TestNewWithProxyTable(t *testing.T) {
	srv := fakeBotAPI(t, func(method string) (int, any) {
		if method == "sendMessage" {
			return 200, map[string]any{"message_id": 7}
		}
		return 200, true
	})

	tests := []struct {
		name     string
		proxyURL string
		wantErr  string // substring; "" = no error
	}{
		{name: "empty keeps direct path", proxyURL: "", wantErr: ""},
		{name: "whitespace keeps direct path", proxyURL: "   ", wantErr: ""},
		{name: "valid socks5", proxyURL: "socks5://127.0.0.1:1", wantErr: ""},
		{name: "malformed url", proxyURL: "socks5://[::bad", wantErr: "parse"},
		{name: "unsupported scheme", proxyURL: "ftp://proxy.invalid:21", wantErr: "unknown scheme"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewWithProxy("123:test", srv.URL, tt.proxyURL, nil)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				if c != nil {
					t.Fatal("client must be nil on error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c == nil || c.Bot == nil {
				t.Fatal("expected client with bot")
			}
		})
	}
}

// (b) end-to-end: with a valid SOCKS5 proxy the sendMessage request must physically
// traverse the proxy (stub CONNECT counter increments) and still hit the mock API.
func TestProxyTrafficFlowsThroughSocks5(t *testing.T) {
	srv := fakeBotAPI(t, func(method string) (int, any) {
		if method == "sendMessage" {
			return 200, map[string]any{"message_id": 99}
		}
		return 200, true
	})
	pxURL, connects := stubSOCKS5(t)

	c, err := NewWithProxy("123:test", srv.URL, pxURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := c.SendMessage(ctx, int64(1), 0, "via proxy")
	if err != nil || id != 99 {
		t.Fatalf("SendMessage via proxy: id=%d err=%v", id, err)
	}
	if *connects == 0 {
		t.Fatal("expected at least one CONNECT through the SOCKS5 proxy")
	}
}

// (d) empty proxy through New() = legacy constructor unchanged (TG_API_URL mock green).
func TestNewLegacyPathUnchanged(t *testing.T) {
	srv := fakeBotAPI(t, func(method string) (int, any) {
		if method == "sendMessage" {
			return 200, map[string]any{"message_id": 5}
		}
		return 200, true
	})
	c := newTestClient(t, srv)
	id, err := c.SendMessage(context.Background(), int64(1), 0, "direct")
	if err != nil || id != 5 {
		t.Fatalf("legacy New path: id=%d err=%v", id, err)
	}
}

// socks5Client attaches a custom DialContext (proxy dialer) to the transport.
func TestSocks5ClientAttachesDialer(t *testing.T) {
	pxURL, _ := stubSOCKS5(t)
	hc, err := socks5Client(pxURL)
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := hc.Transport.(*http.Transport)
	if !ok || tr.DialContext == nil {
		t.Fatal("expected transport with proxy DialContext attached")
	}
	if _, err := socks5Client("socks5://:::not-a-url"); err == nil {
		t.Fatal("expected error for malformed proxy URL")
	}
}
