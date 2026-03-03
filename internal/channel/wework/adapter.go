// Package wework implements channel.Channel for WeCom (企业微信) bots.
//
// WeCom uses HTTP webhook callbacks — requires a public IP or reverse proxy.
// Incoming messages are delivered via POST to the configured webhook path.
// Replies are sent via the WeCom REST API.
//
// Since WeCom does not support editing bot messages, UpdateText buffers
// the output and Seal() sends the final accumulated text as a new message.
package wework

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bishenghua/lazycoding/internal/channel"
	"github.com/bishenghua/lazycoding/internal/config"
	"github.com/bishenghua/lazycoding/internal/transcribe"
)

const (
	wwAPIBase   = "https://qyapi.weixin.qq.com/cgi-bin"
	wwMaxMsgLen = 3000 // WeCom markdown message limit
)

// Adapter implements channel.Channel for WeCom bots.
type Adapter struct {
	cfg    *config.WeWorkConfig
	appCfg *config.Config
	tr     transcribe.Transcriber // nil = voice not supported
	aesKey []byte                 // 32-byte AES key derived from EncodingAESKey

	tokenMu  sync.Mutex
	token    string
	tokenExp time.Time

	events chan channel.InboundEvent
}

// New creates a WeCom Adapter and validates credentials.
// tr may be nil to disable voice transcription.
func New(cfg *config.Config, tr transcribe.Transcriber) (*Adapter, error) {
	if cfg.WeWork.EncodingAESKey == "" {
		return nil, fmt.Errorf("wework: encoding_aes_key is required")
	}
	// AES key: base64decode(EncodingAESKey + "=") → 32 bytes
	aesKey, err := base64.StdEncoding.DecodeString(cfg.WeWork.EncodingAESKey + "=")
	if err != nil || len(aesKey) != 32 {
		return nil, fmt.Errorf("wework: invalid encoding_aes_key (must be 43 base64 chars)")
	}
	a := &Adapter{
		cfg:    &cfg.WeWork,
		appCfg: cfg,
		tr:     tr,
		aesKey: aesKey,
		events: make(chan channel.InboundEvent, 16),
	}
	if _, err := a.getToken(context.Background()); err != nil {
		return nil, fmt.Errorf("wework credential check: %w", err)
	}
	slog.Info("wework adapter ready (webhook mode)",
		"listen", cfg.WeWork.ListenAddr,
		"path", cfg.WeWork.WebhookPath)
	return a, nil
}

// ── channel.Channel ───────────────────────────────────────────────────────────

func (a *Adapter) Events(ctx context.Context) <-chan channel.InboundEvent {
	mux := http.NewServeMux()
	mux.HandleFunc(a.cfg.WebhookPath, func(w http.ResponseWriter, r *http.Request) {
		a.handleWebhook(ctx, w, r)
	})
	srv := &http.Server{Addr: a.cfg.ListenAddr, Handler: mux}
	go func() {
		slog.Info("wework webhook listening", "addr", a.cfg.ListenAddr, "path", a.cfg.WebhookPath)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("wework webhook server", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx) //nolint:errcheck
		close(a.events)
	}()
	return a.events
}

// SendText sends a "thinking" message immediately; the handle buffers the reply.
func (a *Adapter) SendText(ctx context.Context, conversationID, text string) (channel.MessageHandle, error) {
	md := htmlToMarkdown(text)
	if md != "" {
		a.sendMsg(ctx, conversationID, md) //nolint:errcheck
	}
	return &wwHandle{adapter: a, userID: conversationID}, nil
}

// UpdateText buffers text; Seal() sends the final accumulated text.
func (a *Adapter) UpdateText(_ context.Context, handle channel.MessageHandle, text string) error {
	h, ok := handle.(*wwHandle)
	if !ok {
		return fmt.Errorf("wework: unexpected handle type %T", handle)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.sealed {
		h.pending = text
	}
	return nil
}

// SendTyping is a no-op — WeCom has no typing indicator API.
func (a *Adapter) SendTyping(_ context.Context, _ string) error { return nil }

// SendKeyboard sends text immediately (WeCom inline keyboards not supported).
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
	return a.sendMsg(ctx, conversationID, htmlToMarkdown(caption))
}

// ── Handle ────────────────────────────────────────────────────────────────────

type wwHandle struct {
	adapter *Adapter
	userID  string
	mu      sync.Mutex
	sealed  bool
	pending string
}

func (h *wwHandle) Seal() {
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
	h.adapter.sendMsg(ctx, h.userID, htmlToMarkdown(text)) //nolint:errcheck
}

// ── WeCom REST API ────────────────────────────────────────────────────────────

func (a *Adapter) sendMsg(ctx context.Context, toUser, markdown string) error {
	for _, chunk := range splitText(markdown, wwMaxMsgLen) {
		if err := a.sendMsgChunk(ctx, toUser, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (a *Adapter) sendMsgChunk(ctx context.Context, toUser, markdown string) error {
	token, err := a.getToken(ctx)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"touser":  toUser,
		"msgtype": "markdown",
		"agentid": a.cfg.AgentID,
		"markdown": map[string]string{
			"content": markdown,
		},
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/message/send?access_token=%s", wwAPIBase, token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("wework sendMsg: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err == nil && res.ErrCode != 0 {
		slog.Warn("wework sendMsg error", "code", res.ErrCode, "msg", res.ErrMsg)
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
	url := fmt.Sprintf("%s/gettoken?corpid=%s&corpsecret=%s", wwAPIBase, a.cfg.CorpID, a.cfg.AgentSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("wework get token: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var res struct {
		ErrCode     int    `json:"errcode"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("wework token decode: %w", err)
	}
	if res.ErrCode != 0 || res.AccessToken == "" {
		return "", fmt.Errorf("wework get token failed: code=%d body=%s", res.ErrCode, string(raw))
	}
	a.token = res.AccessToken
	expSec := res.ExpiresIn
	if expSec <= 0 {
		expSec = 7200
	}
	a.tokenExp = time.Now().Add(time.Duration(expSec-300) * time.Second)
	slog.Debug("wework token refreshed", "expires_in", expSec)
	return a.token, nil
}

// ── Webhook handling ──────────────────────────────────────────────────────────

// wxXMLMessage is the decrypted WeCom message XML structure.
type wxXMLMessage struct {
	ToUserName   string `xml:"ToUserName"`
	FromUserName string `xml:"FromUserName"`
	CreateTime   int64  `xml:"CreateTime"`
	MsgType      string `xml:"MsgType"`
	Content      string `xml:"Content"`
	MsgID        string `xml:"MsgId"`
	AgentID      int    `xml:"AgentID"`
	// Image fields.
	PicUrl string `xml:"PicUrl"`
	// Media fields (voice, video, file, image).
	MediaId string `xml:"MediaId"`
	// Voice-specific fields.
	Format      string `xml:"Format"`      // amr, speex, etc.
	Recognition string `xml:"Recognition"` // WeCom auto-transcription (optional)
	// File-specific fields.
	FileName string `xml:"FileName"`
}

// wxEncryptedMsg wraps the encrypted message in WeCom's callback format.
type wxEncryptedMsg struct {
	ToUserName   string `xml:"ToUserName"`
	Encrypt      string `xml:"Encrypt"`
	MsgSignature string `xml:"MsgSignature"`
	TimeStamp    string `xml:"TimeStamp"`
	Nonce        string `xml:"Nonce"`
}

func (a *Adapter) handleWebhook(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	msgSig := q.Get("msg_signature")
	timestamp := q.Get("timestamp")
	nonce := q.Get("nonce")

	switch r.Method {
	case http.MethodGet:
		// URL verification: decrypt echostr and return plaintext.
		echostr := q.Get("echostr")
		if echostr == "" {
			http.Error(w, "missing echostr", http.StatusBadRequest)
			return
		}
		if !a.verifySignature(msgSig, timestamp, nonce, echostr) {
			http.Error(w, "invalid signature", http.StatusForbidden)
			return
		}
		plain, _, err := a.decryptMsg(echostr)
		if err != nil {
			slog.Warn("wework echostr decrypt failed", "err", err)
			http.Error(w, "decrypt failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(plain) //nolint:errcheck

	case http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		// Acknowledge immediately.
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("")) //nolint:errcheck

		go a.processPostMessage(ctx, body, msgSig, timestamp, nonce)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *Adapter) processPostMessage(ctx context.Context, body []byte, msgSig, timestamp, nonce string) {
	var enc wxEncryptedMsg
	if err := xml.Unmarshal(body, &enc); err != nil {
		slog.Debug("wework: parse encrypted XML", "err", err)
		return
	}
	if enc.Encrypt == "" {
		// Unencrypted message (unusual for WeCom, but handle gracefully).
		var msg wxXMLMessage
		if err := xml.Unmarshal(body, &msg); err != nil {
			return
		}
		a.dispatchMessage(ctx, &msg)
		return
	}

	if !a.verifySignature(msgSig, timestamp, nonce, enc.Encrypt) {
		slog.Warn("wework: invalid message signature")
		return
	}

	plain, _, err := a.decryptMsg(enc.Encrypt)
	if err != nil {
		slog.Warn("wework: decrypt message failed", "err", err)
		return
	}

	var msg wxXMLMessage
	if err := xml.Unmarshal(plain, &msg); err != nil {
		slog.Debug("wework: parse decrypted XML", "err", err)
		return
	}
	a.dispatchMessage(ctx, &msg)
}

func (a *Adapter) dispatchMessage(ctx context.Context, msg *wxXMLMessage) {
	if msg.FromUserName == "" {
		return
	}

	ev := channel.InboundEvent{
		UserKey:        "ww:" + msg.FromUserName,
		ConversationID: msg.FromUserName, // reply back to this user
	}

	switch msg.MsgType {
	case "text":
		text := strings.TrimSpace(msg.Content)
		if text == "" {
			return
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

	case "voice":
		if !a.handleWWVoice(ctx, msg, &ev) {
			return
		}

	case "image":
		if !a.handleWWImage(ctx, msg, &ev) {
			return
		}

	case "file":
		if !a.handleWWFile(ctx, msg, &ev) {
			return
		}

	default:
		slog.Debug("wework: unsupported message type", "type", msg.MsgType)
		return
	}

	select {
	case a.events <- ev:
	case <-ctx.Done():
	}
}

// downloadMedia fetches a WeCom media file by media_id and saves it to destPath.
func (a *Adapter) downloadMedia(ctx context.Context, mediaID, destPath string) error {
	token, err := a.getToken(ctx)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/media/get?access_token=%s&media_id=%s", wwAPIBase, token, mediaID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()
	// If WeCom returns JSON it means an error occurred.
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "application/json") {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("media download failed: %s", strings.TrimSpace(string(body)))
	}
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

func (a *Adapter) handleWWVoice(ctx context.Context, msg *wxXMLMessage, ev *channel.InboundEvent) bool {
	// Use WeCom's automatic transcription as fallback when no transcriber is configured.
	if a.tr == nil {
		if msg.Recognition != "" {
			ev.Text = msg.Recognition
			ev.IsVoice = true
			return true
		}
		a.sendMsg(ctx, ev.ConversationID, //nolint:errcheck
			"⚠️ Voice transcription is not enabled.\n"+
				"Set `transcription.enabled: true` in config.yaml and restart.\n"+
				"(Or enable voice recognition in the WeCom admin panel for automatic transcription.)")
		return false
	}
	ext := ".amr" // WeCom default voice format
	if msg.Format != "" {
		ext = "." + strings.ToLower(msg.Format)
	}
	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("lc-ww-voice-%d%s", time.Now().UnixNano(), ext))
	if err := a.downloadMedia(ctx, msg.MediaId, tmpFile); err != nil {
		slog.Error("wework voice download failed", "err", err)
		a.sendMsg(ctx, ev.ConversationID, //nolint:errcheck
			fmt.Sprintf("⚠️ Audio download failed: %v", err))
		return false
	}
	defer os.Remove(tmpFile)
	text, err := a.tr.Transcribe(ctx, tmpFile)
	if err != nil {
		slog.Error("wework transcription failed", "err", err)
		a.sendMsg(ctx, ev.ConversationID, //nolint:errcheck
			fmt.Sprintf("⚠️ Transcription failed: %v", err))
		return false
	}
	ev.Text = text
	ev.IsVoice = true
	return true
}

func (a *Adapter) handleWWImage(ctx context.Context, msg *wxXMLMessage, ev *channel.InboundEvent) bool {
	workDir := a.appCfg.WorkDirFor(ev.ConversationID)
	if workDir == "" {
		workDir = "."
	}
	filename := fmt.Sprintf("photo_%s.jpg", time.Now().Format("20060102_150405"))
	destPath := filepath.Join(workDir, filename)
	if err := a.downloadMedia(ctx, msg.MediaId, destPath); err != nil {
		slog.Error("wework image download failed", "err", err)
		a.sendMsg(ctx, ev.ConversationID, //nolint:errcheck
			fmt.Sprintf("⚠️ Image download failed: %v", err))
		return false
	}
	slog.Info("wework image saved", "path", destPath)
	ev.Text = "[File saved to work directory: " + filename + "]"
	return true
}

func (a *Adapter) handleWWFile(ctx context.Context, msg *wxXMLMessage, ev *channel.InboundEvent) bool {
	workDir := a.appCfg.WorkDirFor(ev.ConversationID)
	if workDir == "" {
		workDir = "."
	}
	filename := wwSanitizeFilename(msg.FileName)
	if filename == "" {
		filename = fmt.Sprintf("upload_%d", time.Now().UnixNano())
	}
	destPath := filepath.Join(workDir, filename)
	if err := a.downloadMedia(ctx, msg.MediaId, destPath); err != nil {
		slog.Error("wework file download failed", "err", err)
		a.sendMsg(ctx, ev.ConversationID, //nolint:errcheck
			fmt.Sprintf("⚠️ File download failed: %v", err))
		return false
	}
	slog.Info("wework file saved", "path", destPath)
	ev.Text = "[File saved to work directory: " + filename + "]"
	return true
}

func wwSanitizeFilename(name string) string {
	name = filepath.Base(name)
	name = strings.TrimLeft(name, ".")
	return name
}

// ── AES decryption ────────────────────────────────────────────────────────────

// decryptMsg decrypts a base64-encoded WeCom AES-CBC message.
// Format after decryption: 16-byte random + 4-byte length (big-endian) + XML + corpID
func (a *Adapter) decryptMsg(encoded string) ([]byte, string, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, "", fmt.Errorf("base64 decode: %w", err)
	}

	block, err := aes.NewCipher(a.aesKey)
	if err != nil {
		return nil, "", err
	}
	iv := a.aesKey[:aes.BlockSize]
	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, "", fmt.Errorf("ciphertext not block-aligned")
	}
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(ciphertext, ciphertext)

	// PKCS7 unpad.
	if len(ciphertext) == 0 {
		return nil, "", fmt.Errorf("empty after decrypt")
	}
	pad := int(ciphertext[len(ciphertext)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(ciphertext) {
		return nil, "", fmt.Errorf("invalid PKCS7 padding")
	}
	plaintext := ciphertext[:len(ciphertext)-pad]

	// Parse: 16-byte random + 4-byte length + content + corpID
	if len(plaintext) < 20 {
		return nil, "", fmt.Errorf("decrypted message too short")
	}
	msgLen := int(binary.BigEndian.Uint32(plaintext[16:20]))
	if 20+msgLen > len(plaintext) {
		return nil, "", fmt.Errorf("message length field out of range")
	}
	content := plaintext[20 : 20+msgLen]
	corpID := string(plaintext[20+msgLen:])
	return content, corpID, nil
}

// verifySignature validates the WeCom message signature.
// Signature = SHA1(sorted([token, timestamp, nonce, encrypt]))
func (a *Adapter) verifySignature(sig, timestamp, nonce, encrypt string) bool {
	if a.cfg.Token == "" {
		return true // no token configured, skip verification
	}
	parts := []string{a.cfg.Token, timestamp, nonce, encrypt}
	slices.Sort(parts)
	h := sha1.New()
	h.Write([]byte(strings.Join(parts, "")))
	computed := fmt.Sprintf("%x", h.Sum(nil))
	return computed == sig
}

// ── Rendering ─────────────────────────────────────────────────────────────────

var (
	wwRePreCode    = regexp.MustCompile(`(?s)<pre><code(?:[^>]*)>(.*?)</code></pre>`)
	wwReBold       = regexp.MustCompile(`(?s)<b>(.*?)</b>`)
	wwReItalic     = regexp.MustCompile(`(?s)<i>(.*?)</i>`)
	wwReStrike     = regexp.MustCompile(`(?s)<s>(.*?)</s>`)
	wwReBlockquote = regexp.MustCompile(`(?s)<blockquote>(.*?)</blockquote>`)
	wwReLink       = regexp.MustCompile(`<a href="([^"]*)">(.*?)</a>`)
	wwReCode       = regexp.MustCompile(`<code>(.*?)</code>`)
	wwReTag        = regexp.MustCompile(`<[^>]+>`)
)

func wwHTMLUnescape(s string) string {
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", `"`)
	return s
}

// htmlToMarkdown converts Telegram-style HTML to WeCom Markdown.
func htmlToMarkdown(html string) string {
	if html == "" {
		return ""
	}

	type block struct{ ph, md string }
	var blocks []block

	result := wwRePreCode.ReplaceAllStringFunc(html, func(m string) string {
		inner := wwHTMLUnescape(wwRePreCode.FindStringSubmatch(m)[1])
		ph := "\x00BLOCK" + string(rune(0xE000+len(blocks))) + "\x00"
		blocks = append(blocks, block{ph, "```\n" + inner + "\n```"})
		return ph
	})

	result = wwReLink.ReplaceAllStringFunc(result, func(m string) string {
		sub := wwReLink.FindStringSubmatch(m)
		return "[" + wwHTMLUnescape(sub[2]) + "](" + wwHTMLUnescape(sub[1]) + ")"
	})
	result = wwReBold.ReplaceAllStringFunc(result, func(m string) string {
		return "**" + wwHTMLUnescape(wwReBold.FindStringSubmatch(m)[1]) + "**"
	})
	result = wwReItalic.ReplaceAllStringFunc(result, func(m string) string {
		return "*" + wwHTMLUnescape(wwReItalic.FindStringSubmatch(m)[1]) + "*"
	})
	result = wwReStrike.ReplaceAllStringFunc(result, func(m string) string {
		return "~~" + wwHTMLUnescape(wwReStrike.FindStringSubmatch(m)[1]) + "~~"
	})
	result = wwReBlockquote.ReplaceAllStringFunc(result, func(m string) string {
		return "> " + wwHTMLUnescape(wwReBlockquote.FindStringSubmatch(m)[1])
	})
	result = wwReCode.ReplaceAllStringFunc(result, func(m string) string {
		return "`" + wwHTMLUnescape(wwReCode.FindStringSubmatch(m)[1]) + "`"
	})
	result = wwReTag.ReplaceAllString(result, "")
	result = wwHTMLUnescape(result)

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
