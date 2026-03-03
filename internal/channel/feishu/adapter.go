// Package feishu implements channel.Channel for the Feishu (Lark) bot platform.
//
// Unlike Telegram (which uses long polling), Feishu delivers events via HTTP
// webhook. The adapter starts an HTTP server in Events() and forwards events
// to the returned channel.
package feishu

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bishenghua/lazycoding/internal/channel"
	"github.com/bishenghua/lazycoding/internal/config"
	"github.com/bishenghua/lazycoding/internal/transcribe"
)

const feishuBase = "https://open.feishu.cn/open-apis"

// Adapter implements channel.Channel for Feishu.
type Adapter struct {
	cfg    *config.FeishuConfig
	appCfg *config.Config         // for WorkDirFor lookups
	tr     transcribe.Transcriber // nil = voice not supported

	tokenMu  sync.Mutex
	token    string
	tokenExp time.Time

	seenMu sync.Mutex
	seen   map[string]time.Time // eventID → received time

	events chan channel.InboundEvent
}

// New creates a Feishu Adapter and validates credentials.
// tr may be nil to disable voice transcription.
func New(cfg *config.Config, tr transcribe.Transcriber) (*Adapter, error) {
	a := &Adapter{
		cfg:    &cfg.Feishu,
		appCfg: cfg,
		tr:     tr,
		seen:   make(map[string]time.Time),
		events: make(chan channel.InboundEvent, 16),
	}
	// Validate credentials: token for outbound API calls.
	if _, err := a.getToken(context.Background()); err != nil {
		return nil, fmt.Errorf("feishu credential check failed: %w", err)
	}
	if cfg.Feishu.UseWebhook {
		slog.Info("feishu adapter ready (webhook mode)",
			"listen", cfg.Feishu.ListenAddr,
			"path", cfg.Feishu.WebhookPath)
	} else {
		slog.Info("feishu adapter ready (websocket mode, no public IP required)")
	}
	return a, nil
}

// ── channel.Channel ───────────────────────────────────────────────────────────

// Events starts event delivery and returns the event stream.
//
// Default mode (UseWebhook=false): opens an outbound WebSocket connection to
// Feishu — no public IP or port-forwarding required, works behind NAT exactly
// like Telegram long-polling.
//
// Webhook mode (UseWebhook=true): starts an HTTP server; Feishu must be able
// to reach this machine (public IP or tunnel like ngrok/frp).
func (a *Adapter) Events(ctx context.Context) <-chan channel.InboundEvent {
	go a.cleanSeen(ctx)

	if a.cfg.UseWebhook {
		mux := http.NewServeMux()
		mux.HandleFunc(a.cfg.WebhookPath, func(w http.ResponseWriter, r *http.Request) {
			a.handleWebhook(ctx, w, r)
		})
		srv := &http.Server{Addr: a.cfg.ListenAddr, Handler: mux}
		go func() {
			slog.Info("feishu webhook listening", "addr", a.cfg.ListenAddr, "path", a.cfg.WebhookPath)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("feishu webhook server", "err", err)
			}
		}()
		go func() {
			<-ctx.Done()
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			srv.Shutdown(shutCtx) //nolint:errcheck
			close(a.events)
		}()
	} else {
		go func() {
			slog.Info("feishu ws: starting long connection (no public IP required)")
			a.runWebSocket(ctx)
			close(a.events)
		}()
	}

	return a.events
}

// SendText sends a new Feishu interactive card message.
func (a *Adapter) SendText(ctx context.Context, conversationID, text string) (channel.MessageHandle, error) {
	md := TelegramHTMLToLarkMarkdown(text)
	chunks := SplitText(md)

	msgID, err := a.sendCard(ctx, conversationID, chunks[0], false)
	if err != nil {
		return nil, err
	}
	// Send overflow chunks as separate messages (no editable handle).
	for _, chunk := range chunks[1:] {
		a.sendCard(ctx, conversationID, chunk, false) //nolint:errcheck
	}
	return &fsHandle{adapter: a, messageID: msgID}, nil
}

// UpdateText edits an existing Feishu card message.
func (a *Adapter) UpdateText(ctx context.Context, handle channel.MessageHandle, text string) error {
	h, ok := handle.(*fsHandle)
	if !ok {
		return fmt.Errorf("feishu UpdateText: unexpected handle type %T", handle)
	}
	h.mu.Lock()
	if h.sealed {
		h.mu.Unlock()
		return nil
	}
	hadButton := h.hasKeyboard
	h.hasKeyboard = false
	h.mu.Unlock()

	md := TelegramHTMLToLarkMarkdown(text)
	chunks := SplitText(md)
	// Keep the button only while the task is still running (same logic as Telegram).
	// On first UpdateText after SendKeyboard, hadButton=true → remove it.
	// On subsequent calls, hadButton=false → no button.
	_ = hadButton
	return a.patchCard(ctx, h.messageID, chunks[0], false)
}

// SendTyping is a no-op — Feishu has no typing-indicator API.
func (a *Adapter) SendTyping(_ context.Context, _ string) error { return nil }

// SendKeyboard sends an interactive card with inline buttons.
func (a *Adapter) SendKeyboard(ctx context.Context, conversationID, text string, buttons [][]channel.KeyboardButton) (channel.MessageHandle, error) {
	md := TelegramHTMLToLarkMarkdown(text)
	// Build a flat list of button specs from the 2-D slice.
	var btns []channel.KeyboardButton
	for _, row := range buttons {
		btns = append(btns, row...)
	}
	msgID, err := a.sendCardWithButtons(ctx, conversationID, md, btns)
	if err != nil {
		return nil, err
	}
	return &fsHandle{adapter: a, messageID: msgID, hasKeyboard: true}, nil
}

// AnswerCallback is a no-op — the HTTP 200 response to the webhook IS the
// acknowledgment in Feishu's card action protocol.
func (a *Adapter) AnswerCallback(_ context.Context, _, _ string) error { return nil }

// SendDocument uploads a local file and sends it as a Feishu file message.
func (a *Adapter) SendDocument(ctx context.Context, conversationID, filePath, caption string) error {
	fileKey, err := a.uploadFile(ctx, filePath)
	if err != nil {
		return fmt.Errorf("upload file: %w", err)
	}
	content, _ := json.Marshal(map[string]string{"file_key": fileKey})
	if _, err := a.sendMsg(ctx, conversationID, "file", string(content)); err != nil {
		return err
	}
	if caption != "" {
		md := TelegramHTMLToLarkMarkdown(caption)
		a.sendCard(ctx, conversationID, md, false) //nolint:errcheck
	}
	return nil
}

// ── Webhook handling ──────────────────────────────────────────────────────────

func (a *Adapter) handleWebhook(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	// Decrypt if encryption is configured.
	if a.cfg.EncryptKey != "" {
		var enc struct {
			Encrypt string `json:"encrypt"`
		}
		if json.Unmarshal(body, &enc) == nil && enc.Encrypt != "" {
			decrypted, err := decryptAES(enc.Encrypt, a.cfg.EncryptKey)
			if err != nil {
				slog.Warn("feishu event decrypt failed", "err", err)
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("{}")) //nolint:errcheck
				return
			}
			body = decrypted
		}
	}

	var env feishuEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// URL verification challenge (both flat and schema-2.0 formats).
	if env.Challenge != "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"challenge": env.Challenge}) //nolint:errcheck
		return
	}

	// Respond immediately to prevent Feishu retries.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("{}")) //nolint:errcheck

	// Deduplicate by event ID.
	if env.Header.EventID != "" {
		a.seenMu.Lock()
		_, dup := a.seen[env.Header.EventID]
		if !dup {
			a.seen[env.Header.EventID] = time.Now()
		}
		a.seenMu.Unlock()
		if dup {
			return
		}
	}

	// Dispatch asynchronously so the HTTP handler returns promptly.
	go a.dispatch(ctx, env)
}

func (a *Adapter) dispatch(ctx context.Context, env feishuEnvelope) {
	var ev channel.InboundEvent
	var ok bool

	switch env.Header.EventType {
	case "im.message.receive_v1":
		ev, ok = a.parseMessage(ctx, env.Event)
	case "im.message.action.trigger_v1":
		ev, ok = a.parseAction(env.Event)
	default:
		return
	}

	if !ok {
		return
	}

	select {
	case a.events <- ev:
	case <-ctx.Done():
	}
}

func (a *Adapter) parseMessage(ctx context.Context, raw json.RawMessage) (channel.InboundEvent, bool) {
	var e messageEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		return channel.InboundEvent{}, false
	}

	openID := e.Sender.SenderID.OpenID
	chatID := e.Message.ChatID
	if openID == "" || chatID == "" {
		return channel.InboundEvent{}, false
	}

	// Deduplicate by message_id (stable across event retries and WS reconnects).
	//
	// The envelope-level event_id deduplication in dispatchRaw only works when
	// Feishu retries within the same session.  When runWebSocket reconnects and
	// calls getWSEndpoint() for a fresh URL/session, Feishu treats the new
	// connection as a new subscriber and replays recent messages with brand-new
	// event_ids, bypassing the event_id check.  message_id is the stable
	// identifier for the actual Feishu message and never changes on replay.
	if msgID := e.Message.MessageID; msgID != "" {
		key := "msg:" + msgID // prefix avoids collision with event_ids in the same map
		a.seenMu.Lock()
		_, dup := a.seen[key]
		if !dup {
			a.seen[key] = time.Now()
		}
		a.seenMu.Unlock()
		if dup {
			slog.Debug("feishu: duplicate message dropped", "message_id", msgID)
			return channel.InboundEvent{}, false
		}
	}

	base := channel.InboundEvent{
		UserKey:        "fs:" + openID,
		ConversationID: chatID,
	}

	switch e.Message.MessageType {
	case "text":
		var c struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(e.Message.Content), &c); err != nil {
			return channel.InboundEvent{}, false
		}
		text := strings.TrimSpace(c.Text)
		if text == "" {
			return channel.InboundEvent{}, false
		}
		// Detect slash commands.
		if strings.HasPrefix(text, "/") {
			parts := strings.SplitN(text[1:], " ", 2)
			base.IsCommand = true
			base.Command = strings.ToLower(strings.TrimSpace(parts[0]))
			if len(parts) > 1 {
				base.CommandArgs = strings.TrimSpace(parts[1])
				base.Text = base.CommandArgs
			}
		} else {
			base.Text = text
		}
		return base, true

	case "audio":
		return a.handleAudio(ctx, e, base)

	case "file":
		return a.handleFile(ctx, e, base)

	case "image":
		return a.handleImage(ctx, e, base)

	default:
		slog.Debug("feishu: unsupported message type", "type", e.Message.MessageType)
		return channel.InboundEvent{}, false
	}
}

// handleAudio downloads an audio message and transcribes it.
func (a *Adapter) handleAudio(ctx context.Context, e messageEvent, base channel.InboundEvent) (channel.InboundEvent, bool) {
	if a.tr == nil {
		a.sendCard(ctx, base.ConversationID, //nolint:errcheck
			"⚠️ Voice transcription is not enabled.\n"+
				"Set `transcription.enabled: true` in config.yaml and restart the bot.\n\n"+
				"Recommended (no install required):\n"+
				"  backend: groq\n"+
				"  groq.api_key: <get a free key at console.groq.com>", false)
		return channel.InboundEvent{}, false
	}
	if e.Message.MessageID == "" {
		return channel.InboundEvent{}, false
	}
	var c struct {
		FileKey string `json:"file_key"`
	}
	if err := json.Unmarshal([]byte(e.Message.Content), &c); err != nil || c.FileKey == "" {
		return channel.InboundEvent{}, false
	}
	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("lc-feishu-voice-%d.ogg", time.Now().UnixNano()))
	if err := a.downloadResource(ctx, e.Message.MessageID, c.FileKey, "file", tmpFile); err != nil {
		slog.Error("feishu audio download failed", "err", err)
		a.sendCard(ctx, base.ConversationID, fmt.Sprintf("⚠️ Audio download failed: %v", err), false) //nolint:errcheck
		return channel.InboundEvent{}, false
	}
	defer os.Remove(tmpFile)
	text, err := a.tr.Transcribe(ctx, tmpFile)
	if err != nil {
		slog.Error("feishu transcription failed", "err", err)
		a.sendCard(ctx, base.ConversationID, fmt.Sprintf("⚠️ Transcription failed: %v", err), false) //nolint:errcheck
		return channel.InboundEvent{}, false
	}
	base.Text = text
	base.IsVoice = true
	return base, true
}

// handleFile downloads a file message and saves it to the work directory.
func (a *Adapter) handleFile(ctx context.Context, e messageEvent, base channel.InboundEvent) (channel.InboundEvent, bool) {
	if e.Message.MessageID == "" {
		return channel.InboundEvent{}, false
	}
	var c struct {
		FileKey  string `json:"file_key"`
		FileName string `json:"file_name"`
	}
	if err := json.Unmarshal([]byte(e.Message.Content), &c); err != nil || c.FileKey == "" {
		return channel.InboundEvent{}, false
	}
	workDir := a.appCfg.WorkDirFor(base.ConversationID)
	if workDir == "" {
		workDir = "."
	}
	filename := fsFilename(c.FileName)
	if filename == "" {
		filename = fmt.Sprintf("upload_%d", time.Now().UnixNano())
	}
	destPath := filepath.Join(workDir, filename)
	if err := a.downloadResource(ctx, e.Message.MessageID, c.FileKey, "file", destPath); err != nil {
		slog.Error("feishu file download failed", "err", err)
		a.sendCard(ctx, base.ConversationID, fmt.Sprintf("⚠️ File download failed: %v", err), false) //nolint:errcheck
		return channel.InboundEvent{}, false
	}
	slog.Info("feishu file saved", "path", destPath)
	base.Text = "[File saved to work directory: " + filename + "]"
	return base, true
}

// handleImage downloads an image message and saves it to the work directory.
func (a *Adapter) handleImage(ctx context.Context, e messageEvent, base channel.InboundEvent) (channel.InboundEvent, bool) {
	if e.Message.MessageID == "" {
		return channel.InboundEvent{}, false
	}
	var c struct {
		ImageKey string `json:"image_key"`
	}
	if err := json.Unmarshal([]byte(e.Message.Content), &c); err != nil || c.ImageKey == "" {
		return channel.InboundEvent{}, false
	}
	workDir := a.appCfg.WorkDirFor(base.ConversationID)
	if workDir == "" {
		workDir = "."
	}
	filename := fmt.Sprintf("photo_%s.jpg", time.Now().Format("20060102_150405"))
	destPath := filepath.Join(workDir, filename)
	if err := a.downloadResource(ctx, e.Message.MessageID, c.ImageKey, "image", destPath); err != nil {
		slog.Error("feishu image download failed", "err", err)
		a.sendCard(ctx, base.ConversationID, fmt.Sprintf("⚠️ Image download failed: %v", err), false) //nolint:errcheck
		return channel.InboundEvent{}, false
	}
	slog.Info("feishu image saved", "path", destPath)
	base.Text = "[File saved to work directory: " + filename + "]"
	return base, true
}

// downloadResource fetches a message attachment from Feishu and writes it to destPath.
// resourceType is "file" for documents/audio or "image" for images.
func (a *Adapter) downloadResource(ctx context.Context, messageID, key, resourceType, destPath string) error {
	token, err := a.getToken(ctx)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/im/v1/messages/%s/resources/%s?type=%s",
		feishuBase, messageID, key, resourceType)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
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

// fsFilename strips directory components and leading dots to prevent path traversal.
func fsFilename(name string) string {
	name = filepath.Base(name)
	name = strings.TrimLeft(name, ".")
	return name
}

func (a *Adapter) parseAction(raw json.RawMessage) (channel.InboundEvent, bool) {
	var e actionEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		return channel.InboundEvent{}, false
	}
	chatID := e.Context.OpenChatID
	openID := e.Operator.OpenID
	action := e.Action.Value["action"]
	if chatID == "" || openID == "" || action == "" {
		return channel.InboundEvent{}, false
	}
	return channel.InboundEvent{
		UserKey:        "fs:" + openID,
		ConversationID: chatID,
		IsCallback:     true,
		CallbackData:   action,
	}, true
}

// ── Card builders ─────────────────────────────────────────────────────────────

type cardBody struct {
	Config   cardConfig `json:"config"`
	Elements []any      `json:"elements"`
}
type cardConfig struct {
	WideScreenMode bool `json:"wide_screen_mode"`
}
type cardDiv struct {
	Tag  string   `json:"tag"`
	Text cardText `json:"text"`
}
type cardText struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}
type cardAction struct {
	Tag     string       `json:"tag"`
	Actions []cardButton `json:"actions"`
}
type cardButton struct {
	Tag   string   `json:"tag"`
	Text  cardText `json:"text"`
	Type  string   `json:"type"`
	Value any      `json:"value"`
}

func buildCard(content string, buttons []channel.KeyboardButton) string {
	elements := []any{
		cardDiv{
			Tag:  "div",
			Text: cardText{Tag: "lark_md", Content: content},
		},
	}
	if len(buttons) > 0 {
		var btns []cardButton
		for _, b := range buttons {
			btns = append(btns, cardButton{
				Tag:  "button",
				Text: cardText{Tag: "plain_text", Content: b.Text},
				Type: "danger",
				Value: map[string]string{"action": b.Data},
			})
		}
		elements = append(elements, cardAction{Tag: "action", Actions: btns})
	}
	body := cardBody{
		Config:   cardConfig{WideScreenMode: true},
		Elements: elements,
	}
	b, _ := json.Marshal(body)
	return string(b)
}

// ── Feishu API calls ──────────────────────────────────────────────────────────

func (a *Adapter) sendCard(ctx context.Context, chatID, md string, withCancel bool) (string, error) {
	content := buildCard(md, nil)
	return a.sendMsg(ctx, chatID, "interactive", content)
}

func (a *Adapter) sendCardWithButtons(ctx context.Context, chatID, md string, buttons []channel.KeyboardButton) (string, error) {
	content := buildCard(md, buttons)
	return a.sendMsg(ctx, chatID, "interactive", content)
}

func (a *Adapter) patchCard(ctx context.Context, messageID, md string, withButtons bool) error {
	content := buildCard(md, nil)
	return a.patchMsg(ctx, messageID, content)
}

func (a *Adapter) sendMsg(ctx context.Context, chatID, msgType, content string) (string, error) {
	payload := map[string]string{
		"receive_id": chatID,
		"msg_type":   msgType,
		"content":    content,
	}
	respBytes, err := a.doRequest(ctx, http.MethodPost,
		feishuBase+"/im/v1/messages?receive_id_type=chat_id", payload)
	if err != nil {
		return "", err
	}
	var resp struct {
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return "", fmt.Errorf("sendMsg decode: %w", err)
	}
	return resp.Data.MessageID, nil
}

func (a *Adapter) patchMsg(ctx context.Context, messageID, content string) error {
	payload := map[string]string{"content": content}
	_, err := a.doRequest(ctx, http.MethodPatch,
		feishuBase+"/im/v1/messages/"+messageID, payload)
	return err
}

func (a *Adapter) uploadFile(ctx context.Context, filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("file_type", "stream")          //nolint:errcheck
	mw.WriteField("file_name", filepath.Base(filePath)) //nolint:errcheck
	fw, err := mw.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return "", err
	}
	mw.Close()

	token, err := a.getToken(ctx)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		feishuBase+"/im/v1/files", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	respBody, err := doHTTP(req)
	if err != nil {
		return "", err
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			FileKey string `json:"file_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", fmt.Errorf("uploadFile decode: %w", err)
	}
	if resp.Code != 0 {
		return "", fmt.Errorf("uploadFile code=%d", resp.Code)
	}
	return resp.Data.FileKey, nil
}

// doRequest marshals payload, adds auth, calls the API, and returns the raw body.
// It retries once if the token has expired (code 99991671).
func (a *Adapter) doRequest(ctx context.Context, method, url string, payload any) ([]byte, error) {
	return a.doRequestInner(ctx, method, url, payload, false)
}

func (a *Adapter) doRequestInner(ctx context.Context, method, url string, payload any, retried bool) ([]byte, error) {
	token, err := a.getToken(ctx)
	if err != nil {
		return nil, err
	}

	var bodyR io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		bodyR = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyR)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	raw, err := doHTTP(req)
	if err != nil {
		return nil, err
	}

	var code struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &code); err != nil {
		return nil, fmt.Errorf("decode Feishu response: %w", err)
	}
	if code.Code == 99991671 && !retried {
		// Token expired mid-flight; invalidate and retry once.
		a.tokenMu.Lock()
		a.tokenExp = time.Time{}
		a.tokenMu.Unlock()
		return a.doRequestInner(ctx, method, url, payload, true)
	}
	if code.Code != 0 {
		return nil, fmt.Errorf("feishu API error code=%d msg=%s url=%s", code.Code, code.Msg, url)
	}
	return raw, nil
}

func doHTTP(req *http.Request) ([]byte, error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP %s %s: %w", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return b, nil
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

// refreshToken fetches a new tenant_access_token. Must be called with tokenMu held.
func (a *Adapter) refreshToken(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"app_id":     a.cfg.AppID,
		"app_secret": a.cfg.AppSecret,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		feishuBase+"/auth/v3/tenant_access_token/internal",
		bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	raw, err := doHTTP(req)
	if err != nil {
		return "", err
	}
	var resp struct {
		Code   int    `json:"code"`
		Token  string `json:"tenant_access_token"`
		Expire int    `json:"expire"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("token decode: %w", err)
	}
	if resp.Code != 0 || resp.Token == "" {
		return "", fmt.Errorf("get token failed code=%d", resp.Code)
	}
	a.token = resp.Token
	a.tokenExp = time.Now().Add(time.Duration(resp.Expire-300) * time.Second) // 5-min buffer
	slog.Debug("feishu token refreshed", "expires_in", resp.Expire)
	return a.token, nil
}

// ── Event deduplication ───────────────────────────────────────────────────────

func (a *Adapter) cleanSeen(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			cutoff := time.Now().Add(-10 * time.Minute)
			a.seenMu.Lock()
			for id, t := range a.seen {
				if t.Before(cutoff) {
					delete(a.seen, id)
				}
			}
			a.seenMu.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

// ── AES decryption ────────────────────────────────────────────────────────────

// decryptAES decrypts a Feishu AES-256-CBC encrypted event.
// The AES key is sha256(encryptKey). Ciphertext format: base64(iv[16] + ciphertext).
func decryptAES(encoded, encryptKey string) ([]byte, error) {
	keyHash := sha256.Sum256([]byte(encryptKey))
	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	if len(ciphertext) < aes.BlockSize {
		return nil, fmt.Errorf("ciphertext too short")
	}
	block, err := aes.NewCipher(keyHash[:])
	if err != nil {
		return nil, err
	}
	iv := ciphertext[:aes.BlockSize]
	data := ciphertext[aes.BlockSize:]
	if len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext not block-aligned")
	}
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(data, data)
	// PKCS7 unpad.
	if len(data) == 0 {
		return nil, fmt.Errorf("empty after decrypt")
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > aes.BlockSize {
		return nil, fmt.Errorf("invalid PKCS7 padding")
	}
	return data[:len(data)-pad], nil
}

// ── Handle ────────────────────────────────────────────────────────────────────

type fsHandle struct {
	adapter     *Adapter
	messageID   string
	mu          sync.Mutex
	sealed      bool
	hasKeyboard bool
}

func (h *fsHandle) Seal() {
	h.mu.Lock()
	h.sealed = true
	h.mu.Unlock()
}

func (h *fsHandle) IsSealed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sealed
}

// ── JSON event structs ────────────────────────────────────────────────────────

type feishuEnvelope struct {
	Schema    string          `json:"schema"`
	Challenge string          `json:"challenge"` // URL verification (flat format)
	Header    eventHeader     `json:"header"`
	Event     json.RawMessage `json:"event"`
}

type eventHeader struct {
	EventID   string `json:"event_id"`
	EventType string `json:"event_type"`
}

type messageEvent struct {
	Sender struct {
		SenderID struct {
			OpenID string `json:"open_id"`
		} `json:"sender_id"`
	} `json:"sender"`
	Message struct {
		MessageID   string `json:"message_id"`
		ChatID      string `json:"chat_id"`
		MessageType string `json:"message_type"`
		Content     string `json:"content"` // JSON-encoded string
	} `json:"message"`
}

type actionEvent struct {
	Operator struct {
		OpenID string `json:"open_id"`
	} `json:"operator"`
	Action struct {
		Value map[string]string `json:"value"`
	} `json:"action"`
	Context struct {
		OpenChatID string `json:"open_chat_id"`
	} `json:"context"`
}
