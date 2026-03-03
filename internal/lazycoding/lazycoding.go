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

// discoverLocalSession returns the most recently modified Claude session ID for
// workDir by scanning ~/.claude/projects/<encoded>/*.jsonl.  Claude Code stores
// all sessions (interactive and --print alike) in the same per-project
// directory, so this lets lazycoding resume a session that was started in the
// local CLI without any manual configuration.  Returns "" when nothing is found.
func discoverLocalSession(workDir string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	// Claude encodes the project path by replacing every '/' with '-'.
	encoded := strings.ReplaceAll(workDir, "/", "-")
	projectDir := filepath.Join(home, ".claude", "projects", encoded)

	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return ""
	}

	var newestID string
	var newestMod time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestMod) {
			newestID = strings.TrimSuffix(e.Name(), ".jsonl")
			newestMod = info.ModTime()
		}
	}
	return newestID
}

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

	pendingMu     sync.Mutex
	pending       map[string]*pendingState // key = sessionKey
	runningStatus sync.Map                 // key = sessionKey → string (current rendered HTML)
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
		if sess.ModelOverride != "" {
			extraFlags = applyModelToFlags(extraFlags, sess.ModelOverride)
		}
	} else if workDir != "" {
		// No stored session yet.  Try to discover the most recent session from
		// Claude Code's own project store so we can seamlessly resume a session
		// that was started in the local CLI.
		if id := discoverLocalSession(workDir); id != "" {
			slog.Info("discovered local Claude session", "work_dir", workDir, "session", id)
			claudeSessionID = id
		}
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

	// Only proceed with post-response actions when the context is still alive
	// (i.e. the user did not cancel the request).
	if ctx.Err() != nil {
		return
	}

	if finalText != "" {
		if btns := detectQuickReplies(finalText); btns != nil {
			// Show quick-reply buttons when the agent asked a yes/no question.
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

// applyModelToFlags returns a copy of flags with any existing --model pair
// removed and the given model appended, so the override always wins.
func applyModelToFlags(flags []string, model string) []string {
	out := make([]string, 0, len(flags)+2)
	for i := 0; i < len(flags); i++ {
		if flags[i] == "--model" && i+1 < len(flags) {
			i++ // skip value
			continue
		}
		out = append(out, flags[i])
	}
	return append(out, "--model", model)
}

// effectiveModel returns the model that will be used for convID, checking the
// session override first, then the config extra_flags, then returning a
// placeholder string.
func (lc *Lazycoding) effectiveModel(convID string, sess session.Session) string {
	if sess.ModelOverride != "" {
		return sess.ModelOverride
	}
	flags := lc.cfg.ExtraFlagsFor(convID)
	for i := 0; i+1 < len(flags); i++ {
		if flags[i] == "--model" {
			return flags[i+1]
		}
	}
	return "(Claude default)"
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
	var newUsage *agent.Usage
	var lastFlush time.Time
	doneMarker := "" // set to "\n\n✅" when EventKindResult fires

	// render builds the current message as a full transcript:
	// tool invocations + their outputs, followed by the response text.
	render := func() string {
		var sb strings.Builder

		for _, t := range tools {
			sb.WriteString(t.line)
			sb.WriteString("\n")
			if t.output != "" {
				sb.WriteString("<pre><code>")
				sb.WriteString(tgrender.EscapeHTML(t.output))
				sb.WriteString("</code></pre>\n")
			}
		}

		if textBuf.Len() > 0 {
			if len(tools) > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(tgrender.MarkdownToTelegramHTML(textBuf.String()))
			return sb.String() + doneMarker
		}

		if len(tools) > 0 {
			if doneMarker == "" {
				sb.WriteString("\n<i>(thinking…)</i>")
			}
			return sb.String() + doneMarker
		}

		return "<i>(thinking…)</i>" + doneMarker
	}

	statusKey := lc.sessionKey(ev.ConversationID)
	lc.runningStatus.Store(statusKey, "<i>(thinking…)</i>")
	defer lc.runningStatus.Delete(statusKey)

	doFlush := func() {
		if ctx.Err() != nil {
			return // context cancelled — skip API call
		}
		content := render()
		lc.runningStatus.Store(statusKey, content)
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
			textBuf.WriteString(agEv.Text)
			if lastFlush.IsZero() {
				doFlush() // first text: show immediately
			} else if time.Since(lastFlush) >= throttle {
				doFlush()
			}

		case agent.EventKindToolUse:
			label := fmt.Sprintf("🔧 <i>%s</i>", tgrender.EscapeHTML(agEv.ToolName))
			if summary := formatToolInput(agEv.ToolName, agEv.ToolInput, lc.cfg.WorkDirFor(ev.ConversationID)); summary != "" {
				label = fmt.Sprintf("🔧 <i>%s:</i> <code>%s</code>",
					tgrender.EscapeHTML(agEv.ToolName), tgrender.EscapeHTML(summary))
			}
			entry := toolEntry{id: agEv.ToolUseID, line: label}
			toolIdx[agEv.ToolUseID] = len(tools)
			tools = append(tools, entry)
			if lc.cfg.Log.Verbose {
				convLogTool(agEv.ToolName, agEv.ToolInput, lc.cfg.WorkDirFor(ev.ConversationID))
			}
			doFlush()

		case agent.EventKindToolResult:
			if idx, ok := toolIdx[agEv.ToolUseID]; ok {
				out := truncateOutput(agEv.ToolResult)
				if out != "" {
					tools[idx].output = out
					doFlush()
				}
			}

		case agent.EventKindResult:
			if agEv.SessionID != "" {
				newSessionID = agEv.SessionID
			}
			if agEv.Usage != nil {
				newUsage = agEv.Usage
			}
			if textBuf.Len() == 0 && agEv.Text != "" {
				textBuf.WriteString(agEv.Text)
			}
			doneMarker = "\n\n✅"
			// If the combined transcript would exceed Telegram's limit,
			// keep the tool log in the placeholder and send the response
			// text as one or more new messages so nothing gets truncated.
			finalContent := render()
			if utf8.RuneCountInString(finalContent) > tgrender.MaxMessageLen && textBuf.Len() > 0 {
				// Build the tool log (with outputs) for the placeholder.
				var toolLog strings.Builder
				for _, t := range tools {
					toolLog.WriteString(t.line)
					toolLog.WriteString("\n")
					if t.output != "" {
						toolLog.WriteString("<pre><code>")
						toolLog.WriteString(tgrender.EscapeHTML(t.output))
						toolLog.WriteString("</code></pre>\n")
					}
				}
				placeholder := strings.TrimRight(toolLog.String(), "\n")
				// If tool log alone is still too long, fall back to compact names.
				if utf8.RuneCountInString(placeholder) > tgrender.MaxMessageLen {
					var compact strings.Builder
					for _, t := range tools {
						compact.WriteString(t.line + "\n")
					}
					placeholder = strings.TrimRight(compact.String(), "\n")
				}
				if placeholder == "" {
					placeholder = "<i>(done)</i>"
				}
				lc.ch.UpdateText(ctx, handle, placeholder) //nolint:errcheck
				handle.Seal()
				htmlText := tgrender.MarkdownToTelegramHTML(textBuf.String())
				parts := tgrender.Split(htmlText)
				for i, part := range parts {
					if i == len(parts)-1 {
						part += "\n\n✅"
					}
					lc.ch.SendText(ctx, ev.ConversationID, part) //nolint:errcheck
				}
			} else {
				doFlush()
				handle.Seal()
			}

		case agent.EventKindError:
			if ctx.Err() != nil {
				// Context was cancelled by the user (/cancel, Cancel button, /reset).
				// The confirmation was already sent by handleCallback/handleCommand.
				// Just seal the placeholder silently — no confusing "killed" error message.
				slog.Debug("agent stopped (context cancelled)", "conversation", ev.ConversationID, "reason", ctx.Err())
				handle.Seal()
				break
			}
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

	if newSessionID != "" || newUsage != nil {
		sk := lc.sessionKey(ev.ConversationID)
		existing, _ := lc.store.Get(sk)
		if newSessionID != "" {
			existing.ClaudeSessionID = newSessionID
			slog.Info("session saved",
				"key", sk,
				"conversation", ev.ConversationID,
				"session", newSessionID,
			)
		}
		existing.LastUsed = time.Now()
		if newUsage != nil {
			existing.TotalCostUSD += newUsage.TotalCostUSD
			existing.TotalInputTokens += newUsage.InputTokens + newUsage.CacheReadInputTokens + newUsage.CacheCreationInputTokens
			existing.TotalOutputTokens += newUsage.OutputTokens
		}
		lc.store.Set(sk, existing)
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

	case "status":
		if val, ok := lc.runningStatus.Load(lc.sessionKey(convID)); ok {
			content := "<b>Current task status:</b>\n\n" + val.(string)
			lc.ch.SendText(ctx, convID, content) //nolint:errcheck
		} else {
			lc.ch.SendText(ctx, convID, "No task is currently running.") //nolint:errcheck
		}

	case "compact":
		prompt := "/compact"
		if ev.CommandArgs != "" {
			prompt += " " + ev.CommandArgs
		}
		lc.dispatch(channel.InboundEvent{
			UserKey:        ev.UserKey,
			ConversationID: convID,
			Text:           prompt,
		})

	case "model":
		sessKey := lc.sessionKey(convID)
		existing, _ := lc.store.Get(sessKey)
		if ev.CommandArgs == "" {
			model := lc.effectiveModel(convID, existing)
			lc.ch.SendText(ctx, convID, "Current model: <code>"+tgrender.EscapeHTML(model)+"</code>") //nolint:errcheck
		} else {
			newModel := strings.TrimSpace(ev.CommandArgs)
			existing.ModelOverride = newModel
			lc.store.Set(sessKey, existing)
			lc.ch.SendText(ctx, convID, //nolint:errcheck
				"Model set to <code>"+tgrender.EscapeHTML(newModel)+"</code>. Takes effect on next message.\n"+
					"<i>Use /model to confirm, /reset to clear the model override along with session history.</i>")
		}

	case "cost", "usage":
		sess, ok := lc.store.Get(lc.sessionKey(convID))
		if !ok || (sess.TotalCostUSD == 0 && sess.TotalInputTokens == 0) {
			lc.ch.SendText(ctx, convID, "No usage data yet for this session.") //nolint:errcheck
		} else {
			msg := fmt.Sprintf(
				"<b>Session usage</b>\n"+
					"Input tokens:  <code>%s</code>\n"+
					"Output tokens: <code>%s</code>\n"+
					"Total cost:    <code>$%.5f</code>",
				formatTokens(sess.TotalInputTokens), formatTokens(sess.TotalOutputTokens), sess.TotalCostUSD)
			lc.ch.SendText(ctx, convID, msg) //nolint:errcheck
		}

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
		} else if workDir := lc.cfg.WorkDirFor(convID); workDir != "" {
			if id := discoverLocalSession(workDir); id != "" {
				lc.ch.SendText(ctx, convID, //nolint:errcheck
					"Discovered local session: <code>"+tgrender.EscapeHTML(id)+"</code>\n"+
						"<i>(will be adopted on your first message)</i>")
			} else {
				lc.ch.SendText(ctx, convID, "No active session yet.") //nolint:errcheck
			}
		} else {
			lc.ch.SendText(ctx, convID, "No active session yet.") //nolint:errcheck
		}

	case "resume":
		sessKey := lc.sessionKey(convID)
		sessionID := strings.TrimSpace(ev.CommandArgs)
		if sessionID == "" {
			lc.ch.SendText(ctx, convID, //nolint:errcheck
				"Usage: <code>/resume &lt;session_id&gt;</code>\n"+
					"Resumes a specific Claude session by ID.\n"+
					"Use /session to view the current session ID.")
		} else {
			existing, _ := lc.store.Get(sessKey)
			existing.ClaudeSessionID = sessionID
			lc.store.Set(sessKey, existing)
			lc.ch.SendText(ctx, convID, //nolint:errcheck
				"Session set to <code>"+tgrender.EscapeHTML(sessionID)+"</code>.\n"+
					"<i>Claude will resume this session on your next message.</i>")
		}

	case "workdir":
		workDir := lc.cfg.WorkDirFor(convID)
		if workDir == "" {
			workDir = "(lazycoding launch directory)"
		}
		lc.ch.SendText(ctx, convID, "Work dir: <code>"+tgrender.EscapeHTML(workDir)+"</code>") //nolint:errcheck

	case "ls":
		lc.handleLS(ctx, ev)

	case "tree":
		lc.handleTree(ctx, ev)

	case "cat":
		lc.handleCat(ctx, ev)

	case "download":
		lc.handleDownload(ctx, ev)

	case "help":
		help := "<b>lazycoding</b>\n\n" +
			"<b>Input types:</b>\n" +
			"• Text message → sent directly to Claude\n" +
			"• Voice message → transcribed, then sent to Claude\n" +
			"• File / photo → saved to work dir, Claude is notified\n\n" +
			"<b>Session commands:</b>\n" +
			"/status              – show what Claude is doing right now\n" +
			"/cancel              – stop current task (session is kept)\n" +
			"/reset               – clear session history and start fresh\n" +
			"/compact [hint]      – compress session context\n" +
			"/session             – show current Claude session ID\n" +
			"/resume &lt;id&gt;   – resume a specific Claude session by ID\n" +
			"/model [name]        – show or switch the Claude model\n" +
			"/usage               – show token usage and estimated cost\n\n" +
			"<b>Filesystem commands:</b>\n" +
			"/ls [path]           – list directory contents\n" +
			"/tree [path]         – show directory tree (depth 3)\n" +
			"/cat &lt;path&gt;     – view file contents\n" +
			"/download &lt;path&gt; – download a file from the work directory\n\n" +
			"<b>Info commands:</b>\n" +
			"/workdir             – show current work directory\n" +
			"/start               – welcome message\n" +
			"/help                – show this help"
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

// handleLS lists directory contents (like ls -la).
func (lc *Lazycoding) handleLS(ctx context.Context, ev channel.InboundEvent) {
	convID := ev.ConversationID
	workDir := lc.cfg.WorkDirFor(convID)
	if workDir == "" {
		workDir = "."
	}

	rel := strings.TrimSpace(ev.CommandArgs)
	target := workDir
	if rel != "" {
		var err error
		target, err = safeJoin(workDir, rel)
		if err != nil {
			lc.ch.SendText(ctx, convID, "⚠️ Invalid path: "+err.Error()) //nolint:errcheck
			return
		}
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		lc.ch.SendText(ctx, convID, fmt.Sprintf("⚠️ Cannot read directory: %v", err)) //nolint:errcheck
		return
	}

	displayPath := "."
	if rel != "" {
		displayPath = rel
	}

	var sb strings.Builder
	sb.WriteString("<code>")
	sb.WriteString(tgrender.EscapeHTML(displayPath))
	sb.WriteString("/</code>\n<pre>")

	if len(entries) == 0 {
		sb.WriteString("(empty)")
	} else {
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			sb.WriteString(fmt.Sprintf("%-10s %6s %s  %s\n",
				info.Mode().String(),
				formatFileSize(info.Size()),
				info.ModTime().Format("Jan 02 15:04"),
				tgrender.EscapeHTML(name),
			))
		}
	}
	sb.WriteString("</pre>")
	lc.ch.SendText(ctx, convID, sb.String()) //nolint:errcheck
}

// handleTree shows a directory tree up to 3 levels deep.
func (lc *Lazycoding) handleTree(ctx context.Context, ev channel.InboundEvent) {
	convID := ev.ConversationID
	workDir := lc.cfg.WorkDirFor(convID)
	if workDir == "" {
		workDir = "."
	}

	rel := strings.TrimSpace(ev.CommandArgs)
	target := workDir
	if rel != "" {
		var err error
		target, err = safeJoin(workDir, rel)
		if err != nil {
			lc.ch.SendText(ctx, convID, "⚠️ Invalid path: "+err.Error()) //nolint:errcheck
			return
		}
	}

	const maxDepth = 3
	const maxEntries = 150

	// skipDirs contains directory names that are typically large and uninteresting.
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "vendor": true,
		".cache": true, "__pycache__": true, ".next": true,
	}

	count := 0
	var sb strings.Builder

	var walk func(dir, prefix string, depth int)
	walk = func(dir, prefix string, depth int) {
		if depth > maxDepth || count >= maxEntries {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for i, e := range entries {
			if count >= maxEntries {
				sb.WriteString(prefix + "…\n")
				return
			}
			isLast := i == len(entries)-1
			connector := "├── "
			childPrefix := prefix + "│   "
			if isLast {
				connector = "└── "
				childPrefix = prefix + "    "
			}
			name := e.Name()
			if e.IsDir() {
				if skipDirs[name] {
					sb.WriteString(prefix + connector + name + "/  (skipped)\n")
					count++
					continue
				}
				sb.WriteString(prefix + connector + name + "/\n")
				count++
				if depth < maxDepth {
					walk(filepath.Join(dir, name), childPrefix, depth+1)
				}
			} else {
				sb.WriteString(prefix + connector + name + "\n")
				count++
			}
		}
	}

	displayPath := "."
	if rel != "" {
		displayPath = rel
	}
	sb.WriteString(displayPath + "\n")
	walk(target, "", 0)
	if count >= maxEntries {
		sb.WriteString("…(output truncated at " + fmt.Sprint(maxEntries) + " entries)\n")
	}

	lc.ch.SendText(ctx, convID, "<pre>"+tgrender.EscapeHTML(sb.String())+"</pre>") //nolint:errcheck
}

// handleCat displays the contents of a file.
func (lc *Lazycoding) handleCat(ctx context.Context, ev channel.InboundEvent) {
	convID := ev.ConversationID
	rel := strings.TrimSpace(ev.CommandArgs)

	if rel == "" {
		lc.ch.SendText(ctx, convID, //nolint:errcheck
			"Usage: <code>/cat &lt;path&gt;</code>\n"+
				"Example: <code>/cat src/main.go</code>")
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
		lc.ch.SendText(ctx, convID, fmt.Sprintf("⚠️ Not found: <code>%s</code>", tgrender.EscapeHTML(rel))) //nolint:errcheck
		return
	}
	if info.IsDir() {
		lc.ch.SendText(ctx, convID, fmt.Sprintf("⚠️ <code>%s</code> is a directory — use /ls or /tree.", tgrender.EscapeHTML(rel))) //nolint:errcheck
		return
	}

	const maxLines = 200
	const maxBytes = 8000

	data, err := os.ReadFile(absPath)
	if err != nil {
		lc.ch.SendText(ctx, convID, fmt.Sprintf("⚠️ Cannot read file: %v", err)) //nolint:errcheck
		return
	}

	content := string(data)
	truncated := false

	lines := strings.Split(content, "\n")
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		content = strings.Join(lines, "\n")
		truncated = true
	}
	if len(content) > maxBytes {
		content = safeSlice(content, maxBytes)
		truncated = true
	}

	escaped := tgrender.EscapeHTML(content)
	footer := ""
	if truncated {
		footer = "\n<i>(truncated)</i>"
	}
	msg := fmt.Sprintf("<code>%s</code>\n<pre>%s</pre>%s",
		tgrender.EscapeHTML(rel), escaped, footer)

	// Safety net: if still too long, trim the escaped content further.
	if utf8.RuneCountInString(msg) > tgrender.MaxMessageLen {
		limit := tgrender.MaxMessageLen - 100
		escaped = safeSlice(escaped, limit)
		msg = fmt.Sprintf("<code>%s</code>\n<pre>%s</pre>\n<i>(truncated)</i>",
			tgrender.EscapeHTML(rel), escaped)
	}

	lc.ch.SendText(ctx, convID, msg) //nolint:errcheck
}

// formatTokens converts a token count to a compact human-readable string.
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// formatFileSize converts byte count to a short human-readable string.
func formatFileSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fK", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1fM", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.1fG", float64(n)/(1024*1024*1024))
	}
}
