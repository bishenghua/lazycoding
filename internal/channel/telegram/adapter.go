package telegram

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"lazycoding/internal/channel"
	"lazycoding/internal/config"
	"lazycoding/internal/transcribe"
)

// Adapter implements channel.Channel for Telegram.
type Adapter struct {
	api         *tgbotapi.BotAPI
	cfg         *config.Config     // full config (WorkDirFor, etc.)
	allowedSet  map[int64]bool
	transcriber transcribe.Transcriber // nil = voice messages not supported
}

// New creates and validates a Telegram Adapter.
// tr may be nil to disable voice transcription.
func New(cfg *config.Config, tr transcribe.Transcriber) (*Adapter, error) {
	api, err := tgbotapi.NewBotAPI(cfg.Telegram.Token)
	if err != nil {
		return nil, fmt.Errorf("telegram API init: %w", err)
	}
	slog.Info("telegram connected", "username", api.Self.UserName)
	return &Adapter{
		api:         api,
		cfg:         cfg,
		allowedSet:  cfg.AllowedSet(),
		transcriber: tr,
	}, nil
}

// Events returns a channel of inbound Telegram messages, commands, and
// inline keyboard callback queries.
//
// Each update is processed in its own goroutine so that file downloads and
// voice transcription do not block the polling loop.
func (a *Adapter) Events(ctx context.Context) <-chan channel.InboundEvent {
	out := make(chan channel.InboundEvent, 16)

	go func() {
		defer close(out)

		ucfg := tgbotapi.NewUpdate(0)
		ucfg.Timeout = 30
		updates := a.api.GetUpdatesChan(ucfg)

		for {
			select {
			case <-ctx.Done():
				a.api.StopReceivingUpdates()
				return
			case upd, ok := <-updates:
				if !ok {
					return
				}
				go func(u tgbotapi.Update) {
					// Inline keyboard button press.
					if u.CallbackQuery != nil {
						ev := a.toCallbackEvent(u.CallbackQuery)
						select {
						case out <- ev:
						case <-ctx.Done():
						}
						return
					}
					// Regular message / command.
					ev, ok := a.toEvent(ctx, u)
					if !ok {
						return
					}
					select {
					case out <- ev:
					case <-ctx.Done():
					}
				}(upd)
			}
		}
	}()

	return out
}

// toCallbackEvent converts a Telegram CallbackQuery into an InboundEvent.
func (a *Adapter) toCallbackEvent(cq *tgbotapi.CallbackQuery) channel.InboundEvent {
	chatID := cq.Message.Chat.ID
	return channel.InboundEvent{
		UserKey:        fmt.Sprintf("tg:%d", cq.From.ID),
		ConversationID: strconv.FormatInt(chatID, 10),
		IsCallback:     true,
		CallbackID:     cq.ID,
		CallbackData:   cq.Data,
	}
}

// toEvent converts a Telegram update into an InboundEvent.
// Returns (event, false) when the update should be silently ignored.
func (a *Adapter) toEvent(ctx context.Context, upd tgbotapi.Update) (channel.InboundEvent, bool) {
	msg := upd.Message
	if msg == nil {
		return channel.InboundEvent{}, false
	}

	userID := int64(msg.From.ID)
	chatID := msg.Chat.ID
	convID := strconv.FormatInt(chatID, 10)

	if !a.isAllowed(userID) {
		slog.Warn("unauthorized user", "user_id", userID, "username", msg.From.UserName)
		a.sendMsg(chatID, "Sorry, you are not authorised to use this bot.")
		return channel.InboundEvent{}, false
	}

	base := channel.InboundEvent{
		UserKey:        fmt.Sprintf("tg:%d", userID),
		ConversationID: convID,
	}

	// ── Commands ──────────────────────────────────────────────────────────
	if msg.IsCommand() {
		base.IsCommand = true
		base.Command = msg.Command()
		base.CommandArgs = msg.CommandArguments()
		base.Text = msg.CommandArguments()
		return base, true
	}

	// ── Voice message ─────────────────────────────────────────────────────
	if msg.Voice != nil {
		return a.handleVoice(ctx, msg, base)
	}

	// ── Document (any file type) ──────────────────────────────────────────
	if msg.Document != nil {
		return a.handleDocument(ctx, msg, base)
	}

	// ── Photo ─────────────────────────────────────────────────────────────
	if len(msg.Photo) > 0 {
		return a.handlePhoto(ctx, msg, base)
	}

	// ── Plain text ────────────────────────────────────────────────────────
	base.Text = strings.TrimSpace(msg.Text)
	if base.Text == "" {
		return channel.InboundEvent{}, false
	}
	return base, true
}

// ── Media handlers ────────────────────────────────────────────────────────────

func (a *Adapter) handleVoice(ctx context.Context, msg *tgbotapi.Message, base channel.InboundEvent) (channel.InboundEvent, bool) {
	chatID := msg.Chat.ID

	if a.transcriber == nil {
		a.sendMsg(chatID,
			"⚠️ Voice transcription is not enabled.\n"+
				"Set transcription.enabled: true in config.yaml and restart the bot.\n\n"+
				"Recommended (no install required):\n"+
				"  backend: groq\n"+
				"  groq.api_key: <get a free key at console.groq.com>")
		return channel.InboundEvent{}, false
	}

	a.sendTypingRaw(chatID)

	// Download to a temp OGG file.
	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("lc-voice-%d.ogg", time.Now().UnixNano()))
	if err := a.downloadFile(msg.Voice.FileID, tmpFile); err != nil {
		slog.Error("voice download failed", "err", err)
		a.sendMsg(chatID, fmt.Sprintf("⚠️ Voice download failed: %v", err))
		return channel.InboundEvent{}, false
	}
	defer os.Remove(tmpFile)

	text, err := a.transcriber.Transcribe(ctx, tmpFile)
	if err != nil {
		slog.Error("transcription failed", "err", err)
		a.sendMsg(chatID, fmt.Sprintf("⚠️ Transcription failed: %v", err))
		return channel.InboundEvent{}, false
	}

	base.Text = text
	base.IsVoice = true
	return base, true
}

func (a *Adapter) handleDocument(ctx context.Context, msg *tgbotapi.Message, base channel.InboundEvent) (channel.InboundEvent, bool) {
	chatID := msg.Chat.ID
	doc := msg.Document

	workDir := a.cfg.WorkDirFor(base.ConversationID)
	if workDir == "" {
		workDir = "."
	}

	filename := sanitizeFilename(doc.FileName)
	if filename == "" {
		filename = fmt.Sprintf("upload_%d", time.Now().UnixNano())
	}
	destPath := filepath.Join(workDir, filename)

	a.sendTypingRaw(chatID)
	if err := a.downloadFile(doc.FileID, destPath); err != nil {
		slog.Error("document download failed", "err", err)
		a.sendMsg(chatID, fmt.Sprintf("⚠️ File download failed: %v", err))
		return channel.InboundEvent{}, false
	}

	slog.Info("document saved", "path", destPath, "size", doc.FileSize)

	caption := strings.TrimSpace(msg.Caption)
	base.Text = buildUploadPrompt(filename, caption)
	return base, true
}

func (a *Adapter) handlePhoto(ctx context.Context, msg *tgbotapi.Message, base channel.InboundEvent) (channel.InboundEvent, bool) {
	chatID := msg.Chat.ID

	// Telegram provides multiple resolutions; use the largest (last element).
	largest := msg.Photo[len(msg.Photo)-1]

	workDir := a.cfg.WorkDirFor(base.ConversationID)
	if workDir == "" {
		workDir = "."
	}

	filename := fmt.Sprintf("photo_%s.jpg", time.Now().Format("20060102_150405"))
	destPath := filepath.Join(workDir, filename)

	a.sendTypingRaw(chatID)
	if err := a.downloadFile(largest.FileID, destPath); err != nil {
		slog.Error("photo download failed", "err", err)
		a.sendMsg(chatID, fmt.Sprintf("⚠️ Photo download failed: %v", err))
		return channel.InboundEvent{}, false
	}

	slog.Info("photo saved", "path", destPath)

	caption := strings.TrimSpace(msg.Caption)
	base.Text = buildUploadPrompt(filename, caption)
	return base, true
}

// ── Channel interface ─────────────────────────────────────────────────────────

// SendText sends a new Telegram message and returns an editable handle.
func (a *Adapter) SendText(ctx context.Context, conversationID string, text string) (channel.MessageHandle, error) {
	chatID, err := strconv.ParseInt(conversationID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid conversationID %q: %w", conversationID, err)
	}

	chunks := Split(text)
	m := tgbotapi.NewMessage(chatID, chunks[0])
	m.ParseMode = tgbotapi.ModeHTML

	sent, err := a.api.Send(m)
	if err != nil {
		m.ParseMode = ""
		sent, err = a.api.Send(m)
		if err != nil {
			return nil, fmt.Errorf("send message: %w", err)
		}
	}

	for _, chunk := range chunks[1:] {
		extra := tgbotapi.NewMessage(chatID, chunk)
		extra.ParseMode = tgbotapi.ModeHTML
		if _, e := a.api.Send(extra); e != nil {
			extra.ParseMode = ""
			a.api.Send(extra) //nolint:errcheck
		}
	}

	return &tgHandle{api: a.api, chatID: chatID, msgID: sent.MessageID}, nil
}

// UpdateText edits an existing Telegram message.
// On the first call after SendKeyboard, it also removes the inline keyboard.
func (a *Adapter) UpdateText(ctx context.Context, handle channel.MessageHandle, text string) error {
	h, ok := handle.(*tgHandle)
	if !ok {
		return fmt.Errorf("UpdateText: unexpected handle type %T", handle)
	}
	if h.IsSealed() {
		return nil
	}

	text = Truncate(text, MaxMessageLen)
	if text == "" {
		text = "<i>(empty)</i>"
	}

	h.mu.Lock()
	hadKeyboard := h.hasKeyboard
	h.hasKeyboard = false
	h.mu.Unlock()

	edit := tgbotapi.NewEditMessageText(h.chatID, h.msgID, text)
	edit.ParseMode = tgbotapi.ModeHTML
	if hadKeyboard {
		// Remove the inline keyboard while updating the text.
		empty := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
		edit.ReplyMarkup = &empty
	}

	_, err := a.api.Send(edit)
	if err != nil {
		if isNotModified(err) {
			return nil
		}
		// Retry without parse mode.
		edit.ParseMode = ""
		_, err = a.api.Send(edit)
		if err != nil && isNotModified(err) {
			return nil
		}
	}
	return err
}

// SendTyping sends the "typing…" chat action.
func (a *Adapter) SendTyping(ctx context.Context, conversationID string) error {
	chatID, err := strconv.ParseInt(conversationID, 10, 64)
	if err != nil {
		return nil
	}
	action := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
	_, err = a.api.Send(action)
	return err
}

// SendKeyboard sends a message with an inline keyboard.
func (a *Adapter) SendKeyboard(ctx context.Context, conversationID string, text string, buttons [][]channel.KeyboardButton) (channel.MessageHandle, error) {
	chatID, err := strconv.ParseInt(conversationID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid conversationID %q: %w", conversationID, err)
	}

	rows := make([][]tgbotapi.InlineKeyboardButton, len(buttons))
	for i, row := range buttons {
		btns := make([]tgbotapi.InlineKeyboardButton, len(row))
		for j, btn := range row {
			btns[j] = tgbotapi.NewInlineKeyboardButtonData(btn.Text, btn.Data)
		}
		rows[i] = btns
	}
	markup := tgbotapi.NewInlineKeyboardMarkup(rows...)

	m := tgbotapi.NewMessage(chatID, text)
	m.ParseMode = tgbotapi.ModeHTML
	m.ReplyMarkup = markup

	sent, err := a.api.Send(m)
	if err != nil {
		m.ParseMode = ""
		sent, err = a.api.Send(m)
		if err != nil {
			return nil, fmt.Errorf("send keyboard message: %w", err)
		}
	}
	return &tgHandle{api: a.api, chatID: chatID, msgID: sent.MessageID, hasKeyboard: true}, nil
}

// AnswerCallback acknowledges an inline keyboard button press.
func (a *Adapter) AnswerCallback(ctx context.Context, callbackID string, notification string) error {
	resp := tgbotapi.NewCallback(callbackID, notification)
	_, err := a.api.Request(resp)
	return err
}

// SendDocument uploads a local file to the conversation.
func (a *Adapter) SendDocument(ctx context.Context, conversationID string, filePath string, caption string) error {
	chatID, err := strconv.ParseInt(conversationID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid conversationID %q: %w", conversationID, err)
	}

	doc := tgbotapi.NewDocument(chatID, tgbotapi.FilePath(filePath))
	if caption != "" {
		doc.Caption = caption
	}

	_, err = a.api.Send(doc)
	return err
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// downloadFile fetches a Telegram file by its file_id and writes it to destPath.
func (a *Adapter) downloadFile(fileID, destPath string) error {
	url, err := a.api.GetFileDirectURL(fileID)
	if err != nil {
		return fmt.Errorf("get file URL: %w", err)
	}

	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}

// sendMsg sends a plain-text message without error propagation (fire-and-forget).
func (a *Adapter) sendMsg(chatID int64, text string) {
	m := tgbotapi.NewMessage(chatID, text)
	a.api.Send(m) //nolint:errcheck
}

func (a *Adapter) sendTypingRaw(chatID int64) {
	action := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
	a.api.Send(action) //nolint:errcheck
}

// sanitizeFilename strips directory components to prevent path traversal.
func sanitizeFilename(name string) string {
	name = filepath.Base(name)
	name = strings.TrimLeft(name, ".")
	return name
}

// buildUploadPrompt constructs the text sent to Claude after a file is saved.
func buildUploadPrompt(filename, caption string) string {
	s := fmt.Sprintf("[File saved to work directory: %s]", filename)
	if caption != "" {
		s += "\n" + caption
	}
	return s
}

func (a *Adapter) isAllowed(userID int64) bool {
	if len(a.allowedSet) == 0 {
		return true
	}
	return a.allowedSet[userID]
}

func isNotModified(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message is not modified")
}

// tgHandle is the Telegram implementation of channel.MessageHandle.
type tgHandle struct {
	api    *tgbotapi.BotAPI
	chatID int64
	msgID  int

	mu          sync.Mutex
	sealed      bool
	hasKeyboard bool // true when the message currently has an inline keyboard
}

func (h *tgHandle) Seal() {
	h.mu.Lock()
	h.sealed = true
	h.mu.Unlock()
}

func (h *tgHandle) IsSealed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sealed
}
