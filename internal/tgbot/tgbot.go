// Package tgbot wraps go-telegram/bot v1.25.0 into the narrow surface the gate
// needs: long-poll update pump, sendMessage with MessageThreadID, CreateForumTopic,
// getChat / getChatMember, AnswerCallbackQuery, SetMyCommands. It also exposes a
// Sender interface so tests can substitute a Bot-API mock via TG_API_URL / WithServerURL.
// Optional SOCKS5 egress (TG_BOT_SOCKS5_PROXY_URL) is supported via NewWithProxy.
package tgbot

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"golang.org/x/net/proxy"
)

// ProxyURLEnv is the environment variable naming an optional SOCKS5 proxy used to
// reach the Telegram Bot API (mirrors gorecomendarr's TELEGRAM_BOT_SOCKS5_PROXY_URL).
// Example: socks5://192.168.1.200:9000 (empty/unset = direct connection).
const ProxyURLEnv = "TG_BOT_SOCKS5_PROXY_URL"

// pollTimeout mirrors go-telegram/bot's default long-poll timeout; it is re-supplied
// with the custom client via bot.WithHTTPClient (the option overwrites both fields).
const pollTimeout = 65 * time.Second

// Client wraps the underlying bot and the update handler wire.
type Client struct {
	Bot *bot.Bot
}

// New builds a bot. serverURL, when non-empty (from TG_API_URL env), overrides the
// default api.telegram.org so tests can point at a mock.
func New(token, serverURL string, handler func(ctx context.Context, b *bot.Bot, update *models.Update)) (*Client, error) {
	return NewWithProxy(token, serverURL, "", handler)
}

// NewWithProxy builds a bot like New, additionally routing all Bot-API traffic through
// the SOCKS5 proxy at proxyURL (e.g. "socks5://host:port", optional user:pass). An
// empty/whitespace proxyURL keeps the existing direct-connection path unchanged.
func NewWithProxy(token, serverURL, proxyURL string, handler func(ctx context.Context, b *bot.Bot, update *models.Update)) (*Client, error) {
	opts := []bot.Option{bot.WithSkipGetMe()}
	if serverURL != "" {
		opts = append(opts, bot.WithServerURL(serverURL))
	}
	if handler != nil {
		opts = append(opts, bot.WithDefaultHandler(handler))
	}
	if strings.TrimSpace(proxyURL) != "" {
		client, err := socks5Client(proxyURL)
		if err != nil {
			return nil, err
		}
		opts = append(opts, bot.WithHTTPClient(pollTimeout, client))
	}
	b, err := bot.New(token, opts...)
	if err != nil {
		return nil, fmt.Errorf("new tg bot: %w", err)
	}
	return &Client{Bot: b}, nil
}

// socks5Client builds an http.Client whose transport dials through the SOCKS5 proxy
// parsed from proxyURL via golang.org/x/net/proxy. A malformed URL or a non-SOCKS5
// scheme returns a clean error (no panic).
func socks5Client(proxyURL string) (*http.Client, error) {
	u, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", ProxyURLEnv, err)
	}
	dialer, err := proxy.FromURL(u, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ProxyURLEnv, err)
	}
	d, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("%s: proxy dialer does not support context dialing", ProxyURLEnv)
	}
	transport := &http.Transport{
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{Timeout: pollTimeout, Transport: transport}, nil
}

// SetHandler registers a catch-all handler (receives every update). Used after New so
// the handler can close over objects built later in startup.
func (c *Client) SetHandler(f func(ctx context.Context, b *bot.Bot, update *models.Update)) {
	c.Bot.RegisterHandlerMatchFunc(func(_ *models.Update) bool { return true }, f)
}

// StartBlocking runs the long-poll pump until ctx is cancelled.
func (c *Client) StartBlocking(ctx context.Context) {
	c.Bot.Start(ctx)
}

// SendMessage sends a (possibly topic-scoped) message and returns the message id.
func (c *Client) SendMessage(ctx context.Context, chatID any, threadID int, text string) (int, error) {
	res, err := c.Bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          chatID,
		MessageThreadID: threadID,
		Text:            text,
	})
	if err != nil {
		return 0, err
	}
	return res.ID, nil
}

// SendKeyboard sends a message with an inline keyboard.
func (c *Client) SendKeyboard(ctx context.Context, chatID any, threadID int, text string, kb [][]models.InlineKeyboardButton) error {
	_, err := c.Bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          chatID,
		MessageThreadID: threadID,
		Text:            text,
		ReplyMarkup:     &models.InlineKeyboardMarkup{InlineKeyboard: kb},
	})
	return err
}

// CreateTopic creates a forum topic in chat, returning its message_thread_id.
func (c *Client) CreateTopic(ctx context.Context, chatID any, name string) (int, error) {
	res, err := c.Bot.CreateForumTopic(ctx, &bot.CreateForumTopicParams{
		ChatID: chatID,
		Name:   name,
	})
	if err != nil {
		return 0, err
	}
	return res.MessageThreadID, nil
}

// GetChatInfo returns chat details needed by /connect verification.
type ChatInfo struct {
	IsForum bool
	Title   string
}

// GetChat fetches a chat's info.
func (c *Client) GetChat(ctx context.Context, chatID any) (*ChatInfo, error) {
	ci, err := c.Bot.GetChat(ctx, &bot.GetChatParams{ChatID: chatID})
	if err != nil {
		return nil, err
	}
	return &ChatInfo{IsForum: ci.IsForum, Title: ci.Title}, nil
}

// SenderIsGroupAdmin reports whether the tg user is an administrator (or creator) of chat.
func (c *Client) SenderIsGroupAdmin(ctx context.Context, chatID any, userID int64) bool {
	m, err := c.Bot.GetChatMember(ctx, &bot.GetChatMemberParams{ChatID: chatID, UserID: userID})
	if err != nil {
		return false
	}
	return m.Type == models.ChatMemberTypeOwner || m.Type == models.ChatMemberTypeAdministrator
}

// BotCanManageTopics reports whether the bot itself is an admin with can_manage_topics in chat.
func (c *Client) BotCanManageTopics(ctx context.Context, chatID any) bool {
	admins, err := c.Bot.GetChatAdministrators(ctx, &bot.GetChatAdministratorsParams{ChatID: chatID, ReturnBots: bot.True()})
	if err != nil {
		return false
	}
	for _, a := range admins {
		if a.Administrator != nil && a.Administrator.User.IsBot && a.Administrator.CanManageTopics {
			return true
		}
	}
	return false
}

// AnswerCallback answers a callback query silently, or with an alert.
func (c *Client) AnswerCallback(ctx context.Context, id string, showAlert bool, text string) {
	_, _ = c.Bot.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: id,
		Text:            text,
		ShowAlert:       showAlert,
	})
}

// SetMyCommands registers the bot command list per §12.2.5.
func (c *Client) SetMyCommands(ctx context.Context) {
	_, _ = c.Bot.SetMyCommands(ctx, &bot.SetMyCommandsParams{
		Commands: []models.BotCommand{
			{Command: "start", Description: "Welcome"},
			{Command: "link", Description: "Link a service"},
			{Command: "setup", Description: "Create your forum group"},
			{Command: "connect", Description: "Connect this group (in your group)"},
			{Command: "menu", Description: "Manage services & types"},
			{Command: "status", Description: "Delivery status"},
			{Command: "undelivered", Description: "Failed deliveries"},
			{Command: "help", Description: "Help"},
		},
	})
}
