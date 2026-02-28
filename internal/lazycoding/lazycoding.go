package lazycoding

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bishenghua/lazycoding/internal/agent"
	"github.com/bishenghua/lazycoding/internal/channel"
	tgrender "github.com/bishenghua/lazycoding/internal/channel/telegram"
	"github.com/bishenghua/lazycoding/internal/config"
	"github.com/bishenghua/lazycoding/internal/session"
)

// safeSlice returns s[:n] stepped back to the nearest valid UTF-8 rune
// boundary, preventing invalid sequences when cutting multi-byte characters.
func safeSlice(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Lazycoding orchestrates inbound events, AI streaming, and chat replies.
//
// Session and request-serialization keys are both ConversationID (Telegram chat
// ID), not UserKey.  This implements the "one Claude session per chat" model:
// each configured channel maps to exactly one Claude working directory and
// maintains one continuous session.  Any user in the same chat shares the
// session.
//
// When a new message arrives while Claude is still running, it is queued and
// processed automatically after the current turn completes.
type Lazycoding struct {
	ch    channel.Channel
	ag    agent.Agent
	store session.Store
	cfg   *config.Config

	pendingMu sync.Mutex
	pending   map[string]*pendingState // key = ConversationID
}

// pendingState tracks one in-flight Claude request and its message queue.
type pendingState struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu    sync.Mutex
	queue []channel.InboundEvent // messages arriving while Claude is busy
}

// New creates a Lazycoding instance.
func New(ch channel.Channel, ag agent.Agent, store session.Store, cfg *config.Config) *Lazycoding {
	return &Lazycoding{
		ch:      ch,
		ag:      ag,
		store:   store,
		cfg:     cfg,
		pending: make(map[string]*pendingState),
	}
}

// sessionKey returns the key used for both the pending-request map and the
// session store.  When a work directory is configured for the conversation, the
// directory path is used so that multiple conversations pointing at the same
// project share one Claude session (and are serialised against each other).
// Falls back to the conversation ID when no work directory is set.
func (lc *Lazycoding) sessionKey(convID string) string {
	if d := lc.cfg.WorkDirFor(convID); d != "" {
		return d
	}
	return convID
}

// Run starts the event loop and blocks until ctx is cancelled.
func (lc *Lazycoding) Run(ctx context.Context) error {
	events := lc.ch.Events(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			switch {
			case ev.IsCallback:
				go lc.handleCallback(context.Background(), ev)
			case ev.IsCommand:
				go lc.handleCommand(context.Background(), ev)
			default:
				lc.dispatch(ev)
			}
		}
	}
}

// dispatch either starts a new Claude request or queues the event if one is
// already running for the same session key (work directory or conversation).
func (lc *Lazycoding) dispatch(ev channel.InboundEvent) {
	key := lc.sessionKey(ev.ConversationID)

	lc.pendingMu.Lock()
	if old, ok := lc.pending[key]; ok {
		// Claude is already running for this session key – queue the message.
		old.mu.Lock()
		old.queue = append(old.queue, ev)
		old.mu.Unlock()
		lc.pendingMu.Unlock()
		slog.Debug("message queued", "key", key, "conversation", ev.ConversationID, "text_len", len(ev.Text))
		go lc.ch.SendText(context.Background(), ev.ConversationID, "⏳ Queued — will run after the current task.") //nolint:errcheck
		return
	}

	lc.startRequest(ev)
	lc.pendingMu.Unlock()
}

// startRequest creates a new pendingState and launches the processing goroutine.
// Must be called with pendingMu held.
func (lc *Lazycoding) startRequest(ev channel.InboundEvent) {
	key := lc.sessionKey(ev.ConversationID)
	timeout := time.Duration(lc.cfg.Claude.TimeoutSec) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	done := make(chan struct{})

	state := &pendingState{cancel: cancel, done: done}
	lc.pending[key] = state

	// Keep the Telegram "typing…" indicator alive for the duration of the request.
	// SendTyping goes to the conversation that triggered this particular request.
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				lc.ch.SendTyping(context.Background(), ev.ConversationID) //nolint:errcheck
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		defer func() {
			close(done)
			cancel()

			// After finishing, process the next queued message (if any).
			lc.pendingMu.Lock()
			cur, ok := lc.pending[key]
			if !ok || cur.done != done {
				// Another request replaced ours; nothing to do.
				lc.pendingMu.Unlock()
				return
			}
			delete(lc.pending, key)

			cur.mu.Lock()
			queue := cur.queue
			cur.queue = nil
			cur.mu.Unlock()

			if len(queue) > 0 {
				// Start next queued item without releasing the lock gap.
				lc.startRequest(queue[0])
				// If more than one queued, re-append the remainder.
				if len(queue) > 1 {
					next := lc.pending[key]
					next.mu.Lock()
					next.queue = append(queue[1:], next.queue...)
					next.mu.Unlock()
				}
			}
			lc.pendingMu.Unlock()
		}()
		lc.handleMessage(ctx, ev)
	}()
}

// cancelConversation cancels any in-flight request and clears the queue.
// Returns true if there was something to cancel.
func (lc *Lazycoding) cancelConversation(convID string) bool {
	key := lc.sessionKey(convID)
	lc.pendingMu.Lock()
	defer lc.pendingMu.Unlock()
	state, ok := lc.pending[key]
	if !ok {
		return false
	}
	state.mu.Lock()
	state.queue = nil
	state.mu.Unlock()
	state.cancel()
	delete(lc.pending, key)
	return true
}

// handleMessage processes a normal (non-command) inbound event by streaming
// Claude and forwarding the output back to the chat.
func (lc *Lazycoding) handleMessage(ctx context.Context, ev channel.InboundEvent) {
	text := strings.TrimSpace(ev.Text)
	if text == "" {
		lc.ch.SendText(ctx, ev.ConversationID, "Please send a text message.") //nolint:errcheck
		return
	}

	// For voice messages, echo the recognised text so the user can verify it.
	if ev.IsVoice {
		lc.ch.SendText(ctx, ev.ConversationID, "🎤 <i>Transcribed:</i> "+tgrender.EscapeHTML(ev.Text)) //nolint:errcheck
	}

	lc.ch.SendTyping(ctx, ev.ConversationID) //nolint:errcheck

	// Resolve per-channel settings.
	workDir := lc.cfg.WorkDirFor(ev.ConversationID)
	extraFlags := lc.cfg.ExtraFlagsFor(ev.ConversationID)

	// Look up the ongoing Claude session, keyed by work directory (or conversation
	// ID as fallback).  This ensures all conversations pointing at the same
	// project directory share a single Claude session.
	sessKey := lc.sessionKey(ev.ConversationID)
	var claudeSessionID string
	if sess, ok := lc.store.Get(sessKey); ok {
		claudeSessionID = sess.ClaudeSessionID
	}

	slog.Info("request started",
		"conversation", ev.ConversationID,
		"user", ev.UserKey,
		"session", claudeSessionID,
		"work_dir", workDir,
		"text_len", len(text),
	)
	if lc.cfg.Log.Verbose {
		convLogRecv(ev.ConversationID, ev.UserKey, text)
	}

	// Start the AI stream.
	events, err := lc.ag.Stream(ctx, agent.StreamRequest{
		Prompt:     text,
		SessionID:  claudeSessionID,
		WorkDir:    workDir,
		ExtraFlags: extraFlags,
	})
	if err != nil {
		slog.Error("stream start failed", "conversation", ev.ConversationID, "err", err)
		lc.ch.SendText(ctx, ev.ConversationID, fmt.Sprintf("Error starting Claude: %v", err)) //nolint:errcheck
		return
	}

	// Send the initial "thinking" placeholder with a Cancel button.
	handle, err := lc.ch.SendKeyboard(ctx, ev.ConversationID, "<i>(thinking…)</i>",
		[][]channel.KeyboardButton{{{Text: "✕ Cancel", Data: "cancel"}}})
	if err != nil {
		slog.Error("send placeholder failed", "conversation", ev.ConversationID, "err", err)
		return
	}

	finalText := lc.consumeStream(ctx, ev, handle, events)

	// After the response, show quick-reply buttons if Claude asked a question.
	if ctx.Err() == nil && finalText != "" {
		if btns := detectQuickReplies(finalText); btns != nil {
			lc.ch.SendKeyboard(ctx, ev.ConversationID, "　", //nolint:errcheck
				[][]channel.KeyboardButton{btns})
		}
	}
}

// toolEntry tracks one tool invocation and its output.
type toolEntry struct {
	id     string // tool_use_id for correlation
	line   string // formatted label, e.g. "🔧 <i>Bash:</i> <code>go build</code>"
	output string // truncated tool result; empty = not yet received
}

// isThinkingSignatureError reports whether err is the "Invalid 'signature' in
// 'thinking' block" error that Claude returns when a resumed session contains
// expired extended-thinking signatures.
func isThinkingSignatureError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "signature") && strings.Contains(msg, "thinking")
}

// consumeStream reads agent events and updates the placeholder message in place.
// Returns the final text produced by Claude (for quick-reply detection).
func (lc *Lazycoding) consumeStream(
	ctx context.Context,
	ev channel.InboundEvent,
	handle channel.MessageHandle,
	events <-chan agent.Event,
) string {
	throttle := lc.cfg.Telegram.EditThrottle()

	var textBuf strings.Builder
	var tools []toolEntry
	toolIdx := map[string]int{}
	var newSessionID string
	var lastFlush time.Time
	textStarted := false

	// render builds the current message content as Telegram HTML.
	render := func() string {
		var sb strings.Builder

		if len(tools) > 0 {
			if !textStarted {
				// Processing phase: full tool log with outputs.
				for _, t := range tools {
					sb.WriteString(t.line)
					sb.WriteString("\n")
					if t.output != "" {
						sb.WriteString("<pre><code>")
						sb.WriteString(tgrender.EscapeHTML(t.output))
						sb.WriteString("</code></pre>\n")
					}
				}
				sb.WriteString("\n<i>(thinking…)</i>")
			} else {
				// Response phase: compact tool summary above the text.
				for _, t := range tools {
					sb.WriteString(t.line)
					sb.WriteString("\n")
				}
				sb.WriteString("\n")
				sb.WriteString(tgrender.MarkdownToTelegramHTML(textBuf.String()))
			}
			return sb.String()
		}

		if textBuf.Len() > 0 {
			return tgrender.MarkdownToTelegramHTML(textBuf.String())
		}
		return "<i>(thinking…)</i>"
	}

	doFlush := func() {
		content := render()
		if err := lc.ch.UpdateText(ctx, handle, content); err != nil {
			slog.Warn("update text failed", "conversation", ev.ConversationID, "err", err)
		}
		lastFlush = time.Now()
	}

	for agEv := range events {
		switch agEv.Kind {

		case agent.EventKindInit:
			if agEv.SessionID != "" {
				newSessionID = agEv.SessionID
			}

		case agent.EventKindText:
			if !textStarted {
				textStarted = true
			}
			textBuf.WriteString(agEv.Text)
			if lastFlush.IsZero() {
				doFlush() // first text: show immediately
			} else if time.Since(lastFlush) >= throttle {
				doFlush()
			}

		case agent.EventKindToolUse:
			label := fmt.Sprintf("🔧 <i>%s</i>", tgrender.EscapeHTML(agEv.ToolName))
			if agEv.ToolInput != "" {
				inp := agEv.ToolInput
				if len(inp) > 60 {
					inp = safeSlice(inp, 57) + "…"
				}
				label = fmt.Sprintf("🔧 <i>%s:</i> <code>%s</code>",
					tgrender.EscapeHTML(agEv.ToolName), tgrender.EscapeHTML(inp))
			}
			entry := toolEntry{id: agEv.ToolUseID, line: label}
			toolIdx[agEv.ToolUseID] = len(tools)
			tools = append(tools, entry)
			if lc.cfg.Log.Verbose {
				convLogTool(agEv.ToolName, agEv.ToolInput)
			}
			doFlush()

		case agent.EventKindToolResult:
			if idx, ok := toolIdx[agEv.ToolUseID]; ok {
				out := truncateOutput(agEv.ToolResult)
				if out != "" {
					tools[idx].output = out
					if !textStarted {
						doFlush()
					}
				}
			}

		case agent.EventKindResult:
			if agEv.SessionID != "" {
				newSessionID = agEv.SessionID
			}
			if textBuf.Len() == 0 && agEv.Text != "" {
				textBuf.WriteString(agEv.Text)
			}
			// If the combined tools+text content would exceed Telegram's limit,
			// keep the placeholder for the tool summary and send the response
			// text as one or more new messages so nothing gets truncated.
			finalContent := render()
			if utf8.RuneCountInString(finalContent) > tgrender.MaxMessageLen && textBuf.Len() > 0 {
				var toolsSummary strings.Builder
				for _, t := range tools {
					toolsSummary.WriteString(t.line)
					toolsSummary.WriteString("\n")
				}
				summary := strings.TrimRight(toolsSummary.String(), "\n")
				if summary == "" {
					summary = "<i>(done)</i>"
				}
				lc.ch.UpdateText(ctx, handle, summary) //nolint:errcheck
				handle.Seal()
				htmlText := tgrender.MarkdownToTelegramHTML(textBuf.String())
				for _, part := range tgrender.Split(htmlText) {
					lc.ch.SendText(ctx, ev.ConversationID, part) //nolint:errcheck
				}
			} else {
				doFlush()
				handle.Seal()
			}

		case agent.EventKindError:
			slog.Error("agent error", "conversation", ev.ConversationID, "user", ev.UserKey, "err", agEv.Err)
			if lc.cfg.Log.Verbose {
				convLogError(ev.ConversationID, agEv.Err)
			}
			doFlush()
			handle.Seal()
			msg := fmt.Sprintf("⚠️ Error: %v", agEv.Err)
			if isThinkingSignatureError(agEv.Err) {
				msg = "⚠️ Session contains expired thinking-block signatures. Please send /reset to start a fresh session."
			}
			lc.ch.SendText(ctx, ev.ConversationID, msg) //nolint:errcheck
		}
	}

	// Stream closed without a result event (e.g. context cancelled).
	if ctx.Err() == nil && (textBuf.Len() > 0 || len(tools) > 0) {
		doFlush()
		handle.Seal()
	}

	if lc.cfg.Log.Verbose && textBuf.Len() > 0 {
		convLogSend(textBuf.String())
	}

	if newSessionID != "" {
		sk := lc.sessionKey(ev.ConversationID)
		lc.store.Set(sk, session.Session{
			ClaudeSessionID: newSessionID,
			LastUsed:        time.Now(),
		})
		slog.Info("session saved",
			"key", sk,
			"conversation", ev.ConversationID,
			"session", newSessionID,
		)
	}

	return textBuf.String()
}

// detectQuickReplies returns inline keyboard buttons if the response text looks
// like a yes/no question, or nil if no quick replies are appropriate.
func detectQuickReplies(text string) []channel.KeyboardButton {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	// Find the last non-empty line.
	lines := strings.Split(trimmed, "\n")
	lastLine := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			lastLine = strings.TrimSpace(lines[i])
			break
		}
	}
	if strings.HasSuffix(lastLine, "?") {
		return []channel.KeyboardButton{
			{Text: "✅ Yes", Data: "yes"},
			{Text: "❌ No", Data: "no"},
		}
	}
	return nil
}

// truncateOutput keeps tool output short enough for a chat message.
func truncateOutput(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	const maxChars = 800
	const maxLines = 20
	lines := strings.Split(s, "\n")
	truncatedLines := false
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		truncatedLines = true
	}
	out := strings.Join(lines, "\n")
	if len(out) > maxChars {
		out = safeSlice(out, maxChars)
		if idx := strings.LastIndex(out, "\n"); idx > 0 {
			out = out[:idx]
		}
		out += "\n…"
	} else if truncatedLines {
		out += "\n…"
	}
	return out
}

// handleCallback handles inline keyboard button presses.
func (lc *Lazycoding) handleCallback(ctx context.Context, ev channel.InboundEvent) {
	// Always acknowledge the button press to remove Telegram's loading spinner.
	lc.ch.AnswerCallback(ctx, ev.CallbackID, "") //nolint:errcheck

	data := ev.CallbackData
	convID := ev.ConversationID

	switch data {
	case "cancel":
		if lc.cancelConversation(convID) {
			lc.ch.SendText(ctx, convID, "⏹ Cancelled.") //nolint:errcheck
		}
	default:
		// Treat button data as a text message sent by the user (e.g. "yes", "no").
		lc.dispatch(channel.InboundEvent{
			UserKey:        ev.UserKey,
			ConversationID: convID,
			Text:           data,
		})
	}
}

// handleCommand processes slash commands.
func (lc *Lazycoding) handleCommand(ctx context.Context, ev channel.InboundEvent) {
	convID := ev.ConversationID

	switch ev.Command {
	case "start":
		workDir := lc.cfg.WorkDirFor(convID)
		if workDir == "" {
			workDir = "(not configured, using launch directory)"
		}
		msg := "<b>lazycoding</b> is ready 🛋️\n\n" +
			"Supported input:\n" +
			"• Text message → Claude\n" +
			"• 🎤 Voice message → transcribe → Claude\n" +
			"• 📎 File / photo → saved to work dir → Claude\n\n" +
			"Send /help to see all commands.\n\n" +
			"<i>Work directory:</i> <code>" + tgrender.EscapeHTML(workDir) + "</code>"
		lc.ch.SendText(ctx, convID, msg) //nolint:errcheck

	case "cancel":
		if lc.cancelConversation(convID) {
			lc.ch.SendText(ctx, convID, "⏹ Cancelled.") //nolint:errcheck
		} else {
			lc.ch.SendText(ctx, convID, "No task is currently running.") //nolint:errcheck
		}

	case "reset":
		lc.store.Delete(lc.sessionKey(convID))
		lc.cancelConversation(convID)
		lc.ch.SendText(ctx, convID, "Session reset. Starting fresh.") //nolint:errcheck

	case "session":
		if sess, ok := lc.store.Get(lc.sessionKey(convID)); ok && sess.ClaudeSessionID != "" {
			lc.ch.SendText(ctx, convID, "Current session ID: <code>"+tgrender.EscapeHTML(sess.ClaudeSessionID)+"</code>") //nolint:errcheck
		} else {
			lc.ch.SendText(ctx, convID, "No active session yet.") //nolint:errcheck
		}

	case "workdir":
		workDir := lc.cfg.WorkDirFor(convID)
		if workDir == "" {
			workDir = "(lazycoding launch directory)"
		}
		lc.ch.SendText(ctx, convID, "Work dir: <code>"+tgrender.EscapeHTML(workDir)+"</code>") //nolint:errcheck

	case "download":
		lc.handleDownload(ctx, ev)

	case "help":
		help := "<b>lazycoding</b>\n\n" +
			"<b>Input types:</b>\n" +
			"• Text message → sent directly to Claude\n" +
			"• Voice message → transcribed, then sent to Claude\n" +
			"• File / photo → saved to work dir, Claude is notified\n\n" +
			"<b>Commands:</b>\n" +
			"/start      – welcome message and current work directory\n" +
			"/cancel     – stop current task (session is kept)\n" +
			"/reset      – clear session history and start fresh\n" +
			"/session    – show current Claude session ID\n" +
			"/workdir    – show current work directory\n" +
			"/download &lt;path&gt; – download a file from the work directory\n" +
			"/help       – show this help"
		lc.ch.SendText(ctx, convID, help) //nolint:errcheck

	default:
		lc.ch.SendText(ctx, convID, "Unknown command. Send /help to see available commands.") //nolint:errcheck
	}
}

// handleDownload sends a file from the working directory back to the chat.
func (lc *Lazycoding) handleDownload(ctx context.Context, ev channel.InboundEvent) {
	convID := ev.ConversationID
	rel := strings.TrimSpace(ev.CommandArgs)

	if rel == "" {
		lc.ch.SendText(ctx, convID, //nolint:errcheck
			"Usage: <code>/download &lt;path&gt;</code>\n"+
				"Path is relative to the work directory, e.g.:\n"+
				"<code>/download src/main.go</code>\n"+
				"<code>/download README.md</code>")
		return
	}

	workDir := lc.cfg.WorkDirFor(convID)
	if workDir == "" {
		workDir = "."
	}

	absPath, err := safeJoin(workDir, rel)
	if err != nil {
		lc.ch.SendText(ctx, convID, "⚠️ Invalid path: "+err.Error()) //nolint:errcheck
		return
	}

	info, err := os.Stat(absPath)
	if err != nil {
		lc.ch.SendText(ctx, convID, fmt.Sprintf("⚠️ File not found: <code>%s</code>", tgrender.EscapeHTML(rel))) //nolint:errcheck
		return
	}
	if info.IsDir() {
		lc.ch.SendText(ctx, convID, fmt.Sprintf("⚠️ <code>%s</code> is a directory; please specify a file path", tgrender.EscapeHTML(rel))) //nolint:errcheck
		return
	}

	slog.Info("sending document", "conversation", convID, "path", absPath)
	if err := lc.ch.SendDocument(ctx, convID, absPath, rel); err != nil {
		slog.Error("send document failed", "err", err)
		lc.ch.SendText(ctx, convID, fmt.Sprintf("⚠️ Failed to send file: %v", err)) //nolint:errcheck
	}
}

// safeJoin joins base and rel, returning an error if the result escapes base.
func safeJoin(base, rel string) (string, error) {
	rel = filepath.FromSlash(rel)
	abs := filepath.Clean(filepath.Join(base, rel))
	cleanBase := filepath.Clean(base)
	if abs == cleanBase || strings.HasPrefix(abs, cleanBase+string(filepath.Separator)) {
		return abs, nil
	}
	return "", fmt.Errorf("path %q escapes the work directory", rel)
}
