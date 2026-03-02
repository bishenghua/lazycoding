// Package dingtalk implements channel.Channel for DingTalk (钉钉) bots.
//
// DingTalk stream mode opens an outbound WebSocket connection — no public IP
// or port-forwarding required. The bot gets a WebSocket endpoint from DingTalk
// and receives bot message events over it.
//
// Since DingTalk does not support editing bot messages, UpdateText buffers
// the output and Seal() sends the final accumulated text to the sessionWebhook
// URL embedded in the incoming message.
package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/bishenghua/lazycoding/internal/channel"
	"github.com/bishenghua/lazycoding/internal/config"
)

const (
	dtTokenURL      = "https://api.dingtalk.com/v1.0/oauth2/accessToken"
	dtConnectURL    = "https://api.dingtalk.com/v1.0/gateway/connections/open"
	dtMaxMsgLen     = 4000 // DingTalk markdown message max length
)

// Adapter implements channel.Channel for DingTalk bots.
type Adapter struct {
	cfg *config.DingTalkConfig

	tokenMu  sync.Mutex
	token    string
	tokenExp time.Time

	// webhooks stores the latest sessionWebhook per conversationId.
	// Each incoming message carries a fresh webhook URL (valid ~2 hours).
	webhookMu sync.Mutex
	webhooks  map[string]string

	events chan channel.InboundEvent
}

// New creates a DingTalk Adapter and validates credentials.
func New(cfg *config.Config) (*Adapter, error) {
	a := &Adapter{
		cfg:      &cfg.DingTalk,
		webhooks: make(map[string]string),
		events:   make(chan channel.InboundEvent, 16),
	}
	if _, err := a.getToken(context.Background()); err != nil {
		return nil, fmt.Errorf("dingtalk credential check: %w", err)
	}
	slog.Info("dingtalk adapter ready (stream mode, no public IP required)")
	return a, nil
}

// ── channel.Channel ───────────────────────────────────────────────────────────

func (a *Adapter) Events(ctx context.Context) <-chan channel.InboundEvent {
	go func() {
		slog.Info("dingtalk stream: starting")
		a.runStream(ctx)
		close(a.events)
	}()
	return a.events
}

// SendText sends a "thinking" message immediately via the sessionWebhook.
func (a *Adapter) SendText(ctx context.Context, conversationID, text string) (channel.MessageHandle, error) {
	webhook := a.getWebhook(conversationID)
	md := htmlToMarkdown(text)
	if md != "" {
		a.postWebhook(ctx, webhook, md) //nolint:errcheck
	}
	return &dtHandle{adapter: a, conversationID: conversationID, webhook: webhook}, nil
}

// UpdateText buffers text; the final content is sent by Seal().
func (a *Adapter) UpdateText(_ context.Context, handle channel.MessageHandle, text string) error {
	h, ok := handle.(*dtHandle)
	if !ok {
		return fmt.Errorf("dingtalk: unexpected handle type %T", handle)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.sealed {
		h.pending = text
	}
	return nil
}

// SendTyping is a no-op — DingTalk has no typing indicator API.
func (a *Adapter) SendTyping(_ context.Context, _ string) error { return nil }

// SendKeyboard sends text immediately (DingTalk inline keyboards not supported).
func (a *Adapter) SendKeyboard(ctx context.Context, conversationID, text string, _ [][]channel.KeyboardButton) (channel.MessageHandle, error) {
	return a.SendText(ctx, conversationID, text)
}

// AnswerCallback is a no-op.
func (a *Adapter) AnswerCallback(_ context.Context, _, _ string) error { return nil }

// SendDocument sends the caption as a text message (file upload not supported).
func (a *Adapter) SendDocument(ctx context.Context, conversationID, _ string, caption string) error {
	if caption == "" {
		return nil
	}
	webhook := a.getWebhook(conversationID)
	return a.postWebhook(ctx, webhook, htmlToMarkdown(caption))
}

// ── Handle ────────────────────────────────────────────────────────────────────

type dtHandle struct {
	adapter        *Adapter
	conversationID string
	webhook        string
	mu             sync.Mutex
	sealed         bool
	pending        string
}

func (h *dtHandle) Seal() {
	h.mu.Lock()
	if h.sealed {
		h.mu.Unlock()
		return
	}
	h.sealed = true
	text := h.pending
	h.mu.Unlock()

	if text == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Use the latest webhook (may have been updated by a newer message).
	webhook := h.adapter.getWebhook(h.conversationID)
	if webhook == "" {
		webhook = h.webhook
	}
	h.adapter.postWebhook(ctx, webhook, htmlToMarkdown(text)) //nolint:errcheck
}

// ── Webhook tracking ──────────────────────────────────────────────────────────

func (a *Adapter) setWebhook(conversationID, webhook string) {
	a.webhookMu.Lock()
	a.webhooks[conversationID] = webhook
	a.webhookMu.Unlock()
}

func (a *Adapter) getWebhook(conversationID string) string {
	a.webhookMu.Lock()
	defer a.webhookMu.Unlock()
	return a.webhooks[conversationID]
}

// ── Reply via sessionWebhook ──────────────────────────────────────────────────

func (a *Adapter) postWebhook(ctx context.Context, webhookURL, md string) error {
	if webhookURL == "" {
		slog.Warn("dingtalk: no sessionWebhook available")
		return nil
	}
	chunks := splitText(md, dtMaxMsgLen)
	for _, chunk := range chunks {
		payload := map[string]interface{}{
			"msgtype": "markdown",
			"markdown": map[string]string{
				"title": "Reply",
				"text":  chunk,
			},
		}
		body, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("dingtalk webhook post: %w", err)
		}
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			slog.Warn("dingtalk webhook post failed", "status", resp.StatusCode)
		}
	}
	return nil
}

// ── Token management ──────────────────────────────────────────────────────────

func (a *Adapter) getToken(ctx context.Context) (string, error) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	if a.token != "" && time.Now().Before(a.tokenExp) {
		return a.token, nil
	}
	return a.refreshToken(ctx)
}

func (a *Adapter) refreshToken(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"appKey":    a.cfg.AppKey,
		"appSecret": a.cfg.AppSecret,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dtTokenURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("dingtalk get token: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var res struct {
		AccessToken string `json:"accessToken"`
		ExpireIn    int    `json:"expireIn"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("dingtalk token decode: %w", err)
	}
	if res.AccessToken == "" {
		return "", fmt.Errorf("dingtalk: empty access token; body: %s", string(raw))
	}
	a.token = res.AccessToken
	expSec := res.ExpireIn
	if expSec <= 0 {
		expSec = 7200
	}
	a.tokenExp = time.Now().Add(time.Duration(expSec-300) * time.Second)
	slog.Debug("dingtalk token refreshed", "expires_in", expSec)
	return a.token, nil
}

// ── Stream WebSocket ──────────────────────────────────────────────────────────

type streamEndpoint struct {
	Endpoint string `json:"endpoint"`
	Ticket   string `json:"ticket"`
}

func (a *Adapter) getStreamEndpoint(ctx context.Context, token string) (string, error) {
	payload := map[string]interface{}{
		"clientId":     a.cfg.AppKey,
		"clientSecret": a.cfg.AppSecret,
		"subscriptions": []map[string]string{
			{"type": "EVENT", "topic": "/v1.0/im/bot/messages/getAll"},
		},
		"ua":      "lazycoding/1.0",
		"localIp": "127.0.0.1",
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dtConnectURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-acs-dingtalk-access-token", token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("dingtalk open connection: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var ep streamEndpoint
	if err := json.Unmarshal(raw, &ep); err != nil || ep.Endpoint == "" {
		return "", fmt.Errorf("dingtalk get endpoint: body=%s", string(raw))
	}
	return ep.Endpoint, nil
}

// streamFrame is the JSON frame format used by DingTalk stream mode.
type streamFrame struct {
	SpecVersion string            `json:"specVersion"`
	Type        string            `json:"type"`
	Headers     map[string]string `json:"headers"`
	Data        string            `json:"data"`
}

// runStream loops forever, reconnecting on failure.
func (a *Adapter) runStream(ctx context.Context) {
	backoff := 2 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		token, err := a.getToken(ctx)
		if err != nil {
			slog.Error("dingtalk: get token for stream", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, 60*time.Second)
			}
			continue
		}

		endpoint, err := a.getStreamEndpoint(ctx, token)
		if err != nil {
			slog.Error("dingtalk: get stream endpoint", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, 60*time.Second)
			}
			continue
		}
		backoff = 2 * time.Second

		conn, _, err := websocket.DefaultDialer.DialContext(ctx, endpoint, nil)
		if err != nil {
			slog.Error("dingtalk stream: dial", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, 60*time.Second)
			}
			continue
		}
		slog.Info("dingtalk stream: connected")

		if err := a.serveStreamConn(ctx, conn); err != nil && ctx.Err() == nil {
			slog.Warn("dingtalk stream: disconnected, reconnecting", "err", err)
		}
		conn.Close()

		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (a *Adapter) serveStreamConn(ctx context.Context, conn *websocket.Conn) error {
	// writeCh serialises all writes.
	writeCh := make(chan []byte, 8)
	go func() {
		for {
			select {
			case data := <-writeCh:
				conn.WriteMessage(websocket.TextMessage, data) //nolint:errcheck
			case <-ctx.Done():
				return
			}
		}
	}()

	writeFrame := func(f streamFrame) {
		raw, _ := json.Marshal(f)
		select {
		case writeCh <- raw:
		default:
		}
	}

	sendPong := func(messageID string) {
		writeFrame(streamFrame{
			SpecVersion: "1.0",
			Type:        "SYSTEM",
			Headers: map[string]string{
				"contentType": "application/json",
				"messageId":   messageID,
				"topic":       "pong",
			},
			Data: "{}",
		})
	}

	sendACK := func(messageID string) {
		ackData, _ := json.Marshal(map[string]interface{}{
			"code":    200,
			"message": "OK",
			"requestId": messageID,
			"headers": map[string]string{"contentType": "application/json"},
		})
		writeFrame(streamFrame{
			SpecVersion: "1.0",
			Type:        "SYSTEM",
			Headers: map[string]string{
				"contentType": "application/json",
				"messageId":   messageID,
				"topic":       "ack",
			},
			Data: string(ackData),
		})
	}

	errCh := make(chan error, 1)
	frameCh := make(chan streamFrame, 8)

	go func() {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
			var f streamFrame
			if json.Unmarshal(raw, &f) == nil {
				select {
				case frameCh <- f:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			conn.WriteMessage(websocket.CloseMessage, //nolint:errcheck
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return nil

		case err := <-errCh:
			return err

		case f := <-frameCh:
			msgID := f.Headers["messageId"]
			switch f.Type {
			case "SYSTEM":
				switch f.Headers["topic"] {
				case "ping":
					sendPong(msgID)
				case "disconnect":
					slog.Info("dingtalk stream: server requested disconnect")
					return fmt.Errorf("server disconnect")
				}
			case "EVENT":
				sendACK(msgID)
				go a.handleEvent(ctx, f)
			}
		}
	}
}

// ── Event handling ────────────────────────────────────────────────────────────

type dtBotMessage struct {
	ConversationID            string `json:"conversationId"`
	MsgType                   string `json:"msgtype"`
	Text                      struct {
		Content string `json:"content"`
	} `json:"text"`
	SenderStaffID             string `json:"senderStaffId"`
	SenderID                  string `json:"senderId"`
	SessionWebhook            string `json:"sessionWebhook"`
	SessionWebhookExpiredTime int64  `json:"sessionWebhookExpiredTime"`
}

func (a *Adapter) handleEvent(ctx context.Context, f streamFrame) {
	if f.Headers["topic"] != "/v1.0/im/bot/messages/getAll" {
		return
	}
	var msg dtBotMessage
	if err := json.Unmarshal([]byte(f.Data), &msg); err != nil {
		slog.Debug("dingtalk: decode bot message", "err", err)
		return
	}
	if msg.MsgType != "text" || msg.ConversationID == "" {
		return
	}
	if msg.SessionWebhook != "" {
		a.setWebhook(msg.ConversationID, msg.SessionWebhook)
	}

	// DingTalk group messages start with space + @mention content; trim.
	text := strings.TrimSpace(msg.Text.Content)
	if text == "" {
		return
	}

	senderID := msg.SenderStaffID
	if senderID == "" {
		senderID = msg.SenderID
	}

	ev := channel.InboundEvent{
		UserKey:        "dt:" + senderID,
		ConversationID: msg.ConversationID,
	}
	if strings.HasPrefix(text, "/") {
		parts := strings.SplitN(text[1:], " ", 2)
		ev.IsCommand = true
		ev.Command = strings.ToLower(strings.TrimSpace(parts[0]))
		if len(parts) > 1 {
			ev.CommandArgs = strings.TrimSpace(parts[1])
			ev.Text = ev.CommandArgs
		}
	} else {
		ev.Text = text
	}

	select {
	case a.events <- ev:
	case <-ctx.Done():
	}
}

// ── Rendering ─────────────────────────────────────────────────────────────────

var (
	dtRePreCode    = regexp.MustCompile(`(?s)<pre><code(?:[^>]*)>(.*?)</code></pre>`)
	dtReBold       = regexp.MustCompile(`(?s)<b>(.*?)</b>`)
	dtReItalic     = regexp.MustCompile(`(?s)<i>(.*?)</i>`)
	dtReStrike     = regexp.MustCompile(`(?s)<s>(.*?)</s>`)
	dtReBlockquote = regexp.MustCompile(`(?s)<blockquote>(.*?)</blockquote>`)
	dtReLink       = regexp.MustCompile(`<a href="([^"]*)">(.*?)</a>`)
	dtReCode       = regexp.MustCompile(`<code>(.*?)</code>`)
	dtReTag        = regexp.MustCompile(`<[^>]+>`)
)

func dtHTMLUnescape(s string) string {
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", `"`)
	return s
}

// htmlToMarkdown converts Telegram-style HTML to DingTalk Markdown.
func htmlToMarkdown(html string) string {
	if html == "" {
		return ""
	}

	type block struct{ ph, md string }
	var blocks []block

	result := dtRePreCode.ReplaceAllStringFunc(html, func(m string) string {
		inner := dtHTMLUnescape(dtRePreCode.FindStringSubmatch(m)[1])
		ph := "\x00BLOCK" + string(rune(0xE000+len(blocks))) + "\x00"
		blocks = append(blocks, block{ph, "```\n" + inner + "\n```"})
		return ph
	})

	result = dtReLink.ReplaceAllStringFunc(result, func(m string) string {
		sub := dtReLink.FindStringSubmatch(m)
		return "[" + dtHTMLUnescape(sub[2]) + "](" + dtHTMLUnescape(sub[1]) + ")"
	})
	result = dtReBold.ReplaceAllStringFunc(result, func(m string) string {
		return "**" + dtHTMLUnescape(dtReBold.FindStringSubmatch(m)[1]) + "**"
	})
	result = dtReItalic.ReplaceAllStringFunc(result, func(m string) string {
		return "*" + dtHTMLUnescape(dtReItalic.FindStringSubmatch(m)[1]) + "*"
	})
	result = dtReStrike.ReplaceAllStringFunc(result, func(m string) string {
		return "~~" + dtHTMLUnescape(dtReStrike.FindStringSubmatch(m)[1]) + "~~"
	})
	result = dtReBlockquote.ReplaceAllStringFunc(result, func(m string) string {
		return "> " + dtHTMLUnescape(dtReBlockquote.FindStringSubmatch(m)[1])
	})
	result = dtReCode.ReplaceAllStringFunc(result, func(m string) string {
		return "`" + dtHTMLUnescape(dtReCode.FindStringSubmatch(m)[1]) + "`"
	})
	result = dtReTag.ReplaceAllString(result, "")
	result = dtHTMLUnescape(result)

	for _, b := range blocks {
		result = strings.ReplaceAll(result, b.ph, b.md)
	}
	return result
}

// splitText splits text into chunks of at most maxLen runes.
func splitText(text string, maxLen int) []string {
	runes := []rune(text)
	if len(runes) <= maxLen {
		return []string{text}
	}
	var chunks []string
	for len(runes) > 0 {
		if len(runes) <= maxLen {
			chunks = append(chunks, string(runes))
			break
		}
		cut := maxLen
		for i := cut - 1; i > maxLen/2; i-- {
			if runes[i] == '\n' {
				cut = i
				break
			}
		}
		chunks = append(chunks, string(runes[:cut]))
		runes = runes[cut:]
		if len(runes) > 0 && runes[0] == '\n' {
			runes = runes[1:]
		}
	}
	return chunks
}
