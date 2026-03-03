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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/bishenghua/lazycoding/internal/channel"
	"github.com/bishenghua/lazycoding/internal/config"
	"github.com/bishenghua/lazycoding/internal/transcribe"
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
	cfg    *config.QQBotConfig
	appCfg *config.Config
	tr     transcribe.Transcriber // nil = voice not supported

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
// tr may be nil to disable voice transcription.
func New(cfg *config.Config, tr transcribe.Transcriber) (*Adapter, error) {
	a := &Adapter{
		cfg:    &cfg.QQBot,
		appCfg: cfg,
		tr:     tr,
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

type qqAttachment struct {
	ContentType string `json:"content_type"`
	Filename    string `json:"filename"`
	URL         string `json:"url"`
	Size        int    `json:"size"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
}

type groupMsgEvent struct {
	GroupOpenID string `json:"group_openid"`
	Content     string `json:"content"`
	ID          string `json:"id"` // user msg_id for reply reference
	Author      struct {
		MemberOpenID string `json:"member_openid"`
	} `json:"author"`
	Attachments []qqAttachment `json:"attachments"`
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

		ev := channel.InboundEvent{
			UserKey:        "qq:" + e.Author.MemberOpenID,
			ConversationID: e.GroupOpenID,
		}

		// If text is empty and there are attachments, process the first attachment.
		if text == "" && len(e.Attachments) > 0 {
			if !a.handleAttachment(ctx, e.Attachments[0], &ev) {
				return
			}
		} else if text == "" {
			return
		} else if strings.HasPrefix(text, "/") {
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

// handleAttachment processes a QQ Bot attachment and populates ev accordingly.
// Returns true if the event should be forwarded, false to drop it.
func (a *Adapter) handleAttachment(ctx context.Context, att qqAttachment, ev *channel.InboundEvent) bool {
	ct := strings.ToLower(att.ContentType)
	switch {
	case strings.HasPrefix(ct, "audio/"):
		return a.handleAudio(ctx, att, ev)
	case strings.HasPrefix(ct, "image/"):
		return a.handleImage(ctx, att, ev)
	default:
		return a.handleFile(ctx, att, ev)
	}
}

func (a *Adapter) handleAudio(ctx context.Context, att qqAttachment, ev *channel.InboundEvent) bool {
	if a.tr == nil {
		a.sendGroupMsg(ctx, ev.ConversationID, //nolint:errcheck
			"⚠️ Voice transcription is not enabled.\n"+
				"Set transcription.enabled: true in config.yaml and restart.",
			a.getLatestMsgID(ev.ConversationID))
		return false
	}
	ext := qqExtFromContentType(att.ContentType)
	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("lc-qq-voice-%d%s", time.Now().UnixNano(), ext))
	if err := a.downloadURL(ctx, att.URL, tmpFile); err != nil {
		slog.Error("qqbot audio download failed", "err", err)
		a.sendGroupMsg(ctx, ev.ConversationID, //nolint:errcheck
			fmt.Sprintf("⚠️ Audio download failed: %v", err),
			a.getLatestMsgID(ev.ConversationID))
		return false
	}
	defer os.Remove(tmpFile)
	text, err := a.tr.Transcribe(ctx, tmpFile)
	if err != nil {
		slog.Error("qqbot transcription failed", "err", err)
		a.sendGroupMsg(ctx, ev.ConversationID, //nolint:errcheck
			fmt.Sprintf("⚠️ Transcription failed: %v", err),
			a.getLatestMsgID(ev.ConversationID))
		return false
	}
	ev.Text = text
	ev.IsVoice = true
	return true
}

func (a *Adapter) handleImage(ctx context.Context, att qqAttachment, ev *channel.InboundEvent) bool {
	workDir := a.appCfg.WorkDirFor(ev.ConversationID)
	if workDir == "" {
		workDir = "."
	}
	filename := qqSanitizeFilename(att.Filename)
	if filename == "" {
		ext := qqExtFromContentType(att.ContentType)
		filename = fmt.Sprintf("photo_%s%s", time.Now().Format("20060102_150405"), ext)
	}
	destPath := filepath.Join(workDir, filename)
	if err := a.downloadURL(ctx, att.URL, destPath); err != nil {
		slog.Error("qqbot image download failed", "err", err)
		a.sendGroupMsg(ctx, ev.ConversationID, //nolint:errcheck
			fmt.Sprintf("⚠️ Image download failed: %v", err),
			a.getLatestMsgID(ev.ConversationID))
		return false
	}
	slog.Info("qqbot image saved", "path", destPath)
	ev.Text = "[File saved to work directory: " + filename + "]"
	return true
}

func (a *Adapter) handleFile(ctx context.Context, att qqAttachment, ev *channel.InboundEvent) bool {
	workDir := a.appCfg.WorkDirFor(ev.ConversationID)
	if workDir == "" {
		workDir = "."
	}
	filename := qqSanitizeFilename(att.Filename)
	if filename == "" {
		ext := qqExtFromContentType(att.ContentType)
		filename = fmt.Sprintf("upload_%d%s", time.Now().UnixNano(), ext)
	}
	destPath := filepath.Join(workDir, filename)
	if err := a.downloadURL(ctx, att.URL, destPath); err != nil {
		slog.Error("qqbot file download failed", "err", err)
		a.sendGroupMsg(ctx, ev.ConversationID, //nolint:errcheck
			fmt.Sprintf("⚠️ File download failed: %v", err),
			a.getLatestMsgID(ev.ConversationID))
		return false
	}
	slog.Info("qqbot file saved", "path", destPath)
	ev.Text = "[File saved to work directory: " + filename + "]"
	return true
}

// downloadURL fetches a QQ Bot attachment URL with the bot token and saves it to destPath.
func (a *Adapter) downloadURL(ctx context.Context, url, destPath string) error {
	token, err := a.getToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "QQBot "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func qqSanitizeFilename(name string) string {
	name = filepath.Base(name)
	name = strings.TrimLeft(name, ".")
	return name
}

func qqExtFromContentType(contentType string) string {
	ct := strings.ToLower(strings.SplitN(contentType, ";", 2)[0])
	switch ct {
	case "audio/ogg":
		return ".ogg"
	case "audio/amr":
		return ".amr"
	case "audio/aac":
		return ".aac"
	case "audio/mp3", "audio/mpeg":
		return ".mp3"
	case "audio/wav":
		return ".wav"
	case "audio/silk":
		return ".silk"
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ""
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
