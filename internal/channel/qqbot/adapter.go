// Package qqbot implements channel.Channel for QQ group bots.
//
// QQ group bots use an outbound WebSocket gateway — no public IP required.
// The bot connects to wss://api.sgroup.qq.com/websocket and receives
// GROUP_AT_MESSAGE_CREATE events. Replies are sent via the REST API.
//
// Since QQ does not support editing bot messages, UpdateText buffers the
// output and Seal() sends the final accumulated text as a new message.
package qqbot

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
	qqAPIBase     = "https://api.sgroup.qq.com"
	qqTokenURL    = "https://bots.qq.com/app/getAppAccessToken"
	qqGateway     = "wss://api.sgroup.qq.com/websocket"
	qqIntentGroup = 1 << 25 // GROUP_AND_C2C_EVENT
	qqMaxMsgLen   = 1500    // conservative limit for QQ group messages
)

// WebSocket opcodes.
const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatACK   = 11
)

// Adapter implements channel.Channel for QQ group bots.
type Adapter struct {
	cfg *config.QQBotConfig

	tokenMu  sync.Mutex
	token    string
	tokenExp time.Time

	// msgIDs stores the latest user msg_id per group_openid.
	// QQ requires referencing the original user msg_id within 5 minutes to reply.
	msgIDMu sync.Mutex
	msgIDs  map[string]string

	events chan channel.InboundEvent
}

// New creates a QQ Bot Adapter and validates credentials.
func New(cfg *config.Config) (*Adapter, error) {
	a := &Adapter{
		cfg:    &cfg.QQBot,
		msgIDs: make(map[string]string),
		events: make(chan channel.InboundEvent, 16),
	}
	if _, err := a.getToken(context.Background()); err != nil {
		return nil, fmt.Errorf("qqbot credential check: %w", err)
	}
	slog.Info("qqbot adapter ready (websocket mode, no public IP required)")
	return a, nil
}

// ── channel.Channel ───────────────────────────────────────────────────────────

func (a *Adapter) Events(ctx context.Context) <-chan channel.InboundEvent {
	go func() {
		slog.Info("qqbot ws: starting long connection")
		a.runWebSocket(ctx)
		close(a.events)
	}()
	return a.events
}

// SendText sends "thinking" text immediately; the handle buffers the final reply.
func (a *Adapter) SendText(ctx context.Context, conversationID, text string) (channel.MessageHandle, error) {
	msgID := a.getLatestMsgID(conversationID)
	plain := htmlToPlainText(text)
	if plain != "" {
		a.sendGroupMsg(ctx, conversationID, plain, msgID) //nolint:errcheck
	}
	return &qqHandle{adapter: a, groupID: conversationID, origMsgID: msgID}, nil
}

// UpdateText buffers the text; the final content is sent by Seal().
func (a *Adapter) UpdateText(_ context.Context, handle channel.MessageHandle, text string) error {
	h, ok := handle.(*qqHandle)
	if !ok {
		return fmt.Errorf("qqbot: unexpected handle type %T", handle)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.sealed {
		h.pending = text
	}
	return nil
}

// SendTyping is a no-op — QQ has no typing indicator API.
func (a *Adapter) SendTyping(_ context.Context, _ string) error { return nil }

// SendKeyboard sends text immediately (QQ does not support inline keyboards).
func (a *Adapter) SendKeyboard(ctx context.Context, conversationID, text string, _ [][]channel.KeyboardButton) (channel.MessageHandle, error) {
	return a.SendText(ctx, conversationID, text)
}

// AnswerCallback is a no-op.
func (a *Adapter) AnswerCallback(_ context.Context, _, _ string) error { return nil }

// SendDocument sends the caption as a text message (QQ file upload is not supported).
func (a *Adapter) SendDocument(ctx context.Context, conversationID, _ string, caption string) error {
	if caption == "" {
		return nil
	}
	msgID := a.getLatestMsgID(conversationID)
	return a.sendGroupMsg(ctx, conversationID, htmlToPlainText(caption), msgID)
}

// ── Handle ────────────────────────────────────────────────────────────────────

type qqHandle struct {
	adapter   *Adapter
	groupID   string
	origMsgID string
	mu        sync.Mutex
	sealed    bool
	pending   string
}

func (h *qqHandle) Seal() {
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
	h.adapter.sendGroupMsg(ctx, h.groupID, htmlToPlainText(text), h.origMsgID) //nolint:errcheck
}

// ── Message ID tracking ───────────────────────────────────────────────────────

func (a *Adapter) setLatestMsgID(groupID, msgID string) {
	a.msgIDMu.Lock()
	a.msgIDs[groupID] = msgID
	a.msgIDMu.Unlock()
}

func (a *Adapter) getLatestMsgID(groupID string) string {
	a.msgIDMu.Lock()
	defer a.msgIDMu.Unlock()
	return a.msgIDs[groupID]
}

// ── REST API ──────────────────────────────────────────────────────────────────

func (a *Adapter) sendGroupMsg(ctx context.Context, groupOpenID, text, msgID string) error {
	for _, chunk := range splitText(text, qqMaxMsgLen) {
		if err := a.sendGroupMsgChunk(ctx, groupOpenID, chunk, msgID); err != nil {
			return err
		}
	}
	return nil
}

func (a *Adapter) sendGroupMsgChunk(ctx context.Context, groupOpenID, text, msgID string) error {
	payload := map[string]interface{}{
		"content":  text,
		"msg_type": 0, // 0 = text
	}
	if msgID != "" {
		payload["msg_id"] = msgID
	}
	body, _ := json.Marshal(payload)

	token, err := a.getToken(ctx)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/v2/groups/%s/messages", qqAPIBase, groupOpenID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "QQBot "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("qqbot sendMsg: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("qqbot sendMsg failed", "status", resp.StatusCode, "body", string(raw))
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
		"appId":        a.cfg.AppID,
		"clientSecret": a.cfg.ClientSecret,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, qqTokenURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("qqbot get token: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var res struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   string `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("qqbot token decode: %w", err)
	}
	if res.AccessToken == "" {
		return "", fmt.Errorf("qqbot: empty access token; body: %s", string(raw))
	}
	var expSec int
	fmt.Sscanf(res.ExpiresIn, "%d", &expSec) //nolint:errcheck
	if expSec <= 0 {
		expSec = 7200
	}
	a.token = res.AccessToken
	a.tokenExp = time.Now().Add(time.Duration(expSec-300) * time.Second)
	slog.Debug("qqbot token refreshed", "expires_in", expSec)
	return a.token, nil
}

// ── WebSocket ─────────────────────────────────────────────────────────────────

type wsMsg struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  int             `json:"s"` // sequence number (for heartbeat)
	T  string          `json:"t"` // event type (for OP 0 Dispatch)
}

// runWebSocket connects and reconnects until ctx is cancelled.
func (a *Adapter) runWebSocket(ctx context.Context) {
	backoff := 2 * time.Second
	lastSeq := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		token, err := a.getToken(ctx)
		if err != nil {
			slog.Error("qqbot: get token", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, 60*time.Second)
			}
			continue
		}

		conn, _, err := websocket.DefaultDialer.DialContext(ctx, qqGateway, nil)
		if err != nil {
			slog.Error("qqbot ws: dial", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, 60*time.Second)
			}
			continue
		}
		backoff = 2 * time.Second
		slog.Info("qqbot ws: connected")

		lastSeq = a.serveWSConn(ctx, conn, token, lastSeq)
		conn.Close()

		if ctx.Err() != nil {
			return
		}
		slog.Warn("qqbot ws: disconnected, reconnecting")
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// serveWSConn handles one WebSocket session and returns the last sequence number.
func (a *Adapter) serveWSConn(ctx context.Context, conn *websocket.Conn, token string, lastSeq int) int {
	heartbeatInterval := 45 * time.Second

	// writeCh serialises all writes to the connection.
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

	write := func(v interface{}) {
		raw, _ := json.Marshal(v)
		select {
		case writeCh <- raw:
		default:
		}
	}

	msgCh := make(chan wsMsg, 8)
	errCh := make(chan error, 1)
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
			var msg wsMsg
			if json.Unmarshal(raw, &msg) == nil {
				select {
				case msgCh <- msg:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()
	identified := false

	sendHeartbeat := func() {
		var d interface{}
		if lastSeq > 0 {
			d = lastSeq
		}
		write(map[string]interface{}{"op": opHeartbeat, "d": d})
	}

	for {
		select {
		case <-ctx.Done():
			conn.WriteMessage(websocket.CloseMessage, //nolint:errcheck
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return lastSeq

		case err := <-errCh:
			if ctx.Err() == nil {
				slog.Warn("qqbot ws: read error", "err", err)
			}
			return lastSeq

		case <-heartbeatTicker.C:
			sendHeartbeat()

		case msg := <-msgCh:
			if msg.S > 0 {
				lastSeq = msg.S
			}
			switch msg.Op {
			case opHello:
				var hello struct {
					HeartbeatInterval int `json:"heartbeat_interval"`
				}
				if json.Unmarshal(msg.D, &hello) == nil && hello.HeartbeatInterval > 0 {
					heartbeatInterval = time.Duration(hello.HeartbeatInterval) * time.Millisecond
					heartbeatTicker.Reset(heartbeatInterval)
				}
				write(map[string]interface{}{
					"op": opIdentify,
					"d": map[string]interface{}{
						"token":   "QQBot " + token,
						"intents": qqIntentGroup,
						"shard":   []int{0, 1},
					},
				})
				identified = true

			case opHeartbeatACK:
				// Ticker handles timing; nothing extra needed.

			case opHeartbeat:
				sendHeartbeat()

			case opDispatch:
				if identified {
					go a.handleDispatch(ctx, msg)
				}

			case opInvalidSession, opReconnect:
				slog.Warn("qqbot ws: session reset", "op", msg.Op)
				return lastSeq
			}
		}
	}
}

// ── Event handling ────────────────────────────────────────────────────────────

type groupMsgEvent struct {
	GroupOpenID string `json:"group_openid"`
	Content     string `json:"content"`
	ID          string `json:"id"` // user msg_id for reply reference
	Author      struct {
		MemberOpenID string `json:"member_openid"`
	} `json:"author"`
}

func (a *Adapter) handleDispatch(ctx context.Context, msg wsMsg) {
	switch msg.T {
	case "GROUP_AT_MESSAGE_CREATE":
		var e groupMsgEvent
		if err := json.Unmarshal(msg.D, &e); err != nil || e.GroupOpenID == "" {
			return
		}
		a.setLatestMsgID(e.GroupOpenID, e.ID)

		text := strings.TrimSpace(stripAtMention(e.Content))
		if text == "" {
			return
		}
		ev := channel.InboundEvent{
			UserKey:        "qq:" + e.Author.MemberOpenID,
			ConversationID: e.GroupOpenID,
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
}

// stripAtMention removes the leading <@!botID> mention QQ injects in group messages.
var reAtMention = regexp.MustCompile(`^<@!\d+>\s*`)

func stripAtMention(content string) string {
	return strings.TrimSpace(reAtMention.ReplaceAllString(content, ""))
}

// ── Rendering ─────────────────────────────────────────────────────────────────

var reHTMLTag = regexp.MustCompile(`<[^>]+>`)

var htmlEntities = strings.NewReplacer(
	"&amp;", "&",
	"&lt;", "<",
	"&gt;", ">",
	"&quot;", `"`,
	"&#39;", "'",
	"&nbsp;", " ",
)

// htmlToPlainText strips all HTML tags and unescapes entities.
func htmlToPlainText(html string) string {
	text := reHTMLTag.ReplaceAllString(html, "")
	text = htmlEntities.Replace(text)
	return strings.TrimSpace(text)
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
