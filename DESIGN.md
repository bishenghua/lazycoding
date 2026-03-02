# lazycoding – Architecture & Design

## Motivation

Claude Code is a powerful agentic coding tool, but it's bound to a terminal on your development machine. The moment you step away from your desk, you lose access.

lazycoding removes that constraint. It is a **local gateway process** that exposes Claude Code to any supported chat platform. The design follows three principles:

1. **Locality** — Claude Code runs on *your* machine with full access to *your* filesystem. No cloud intermediary touches your source code.
2. **Multiplexing** — one bot process serves many projects. Each Telegram conversation maps to one project directory; conversations are fully isolated from each other.
3. **Extensibility** — every major boundary (chat platform, AI backend, session store, speech-to-text) is abstracted behind an interface, making it straightforward to swap implementations or add new ones.

---

## System Overview

```
┌─────────────────────────────────────────────────────────────┐
│                     Developer's machine                      │
│                                                             │
│  ┌──────────────────────────────────────────────────────┐  │
│  │                    lazycoding                         │  │
│  │                                                      │  │
│  │  ┌──────────────────┐   ┌───────────┐   ┌──────────────┐  │  │
│  │  │    channel/      │   │lazycoding │   │    agent/    │  │  │
│  │  │ tg|fs|qq|dt|ww   │◄──│    core   │──►│    claude    │  │  │
│  │  │    adapters      │   │(dispatch) │   │   runner     │  │  │
│  │  └──────────────────┘   └───────────┘   └──────────────┘  │  │
│  │          │                   │                  │           │  │
│  │   InboundEvent          session.Store      subprocess       │  │
│  │   MessageHandle         FileStore          stream-json      │  │
│  └────────────────────────────────────────────────────────┘  │
│                                                               │
└───────────────────────────────────────────────────────────────┘
         ▲                                        ▼
  Telegram (long-poll)                    Project files
  Feishu / QQ / DingTalk (WebSocket)     /path/to/project/
  WeCom (HTTP webhook)
```

---

## Directory Structure

```
cmd/lazycoding/
  main.go                   entry point: wire dependencies, graceful shutdown

internal/
  config/
    config.go               Config structs, YAML loading, defaults
                            WorkDirFor / ExtraFlagsFor resolution helpers

  agent/
    agent.go                Agent interface, StreamRequest, Event types
    claude/
      runner.go             spawn claude subprocess with correct WorkDir
      parser.go             stream-json JSONL → []agent.Event

  session/
    store.go                Store interface, MemoryStore, FileStore (JSON-backed)

  channel/
    channel.go              Channel interface, InboundEvent, MessageHandle,
                            KeyboardButton, NewMultiAdapter (fan-in + routing)
    telegram/
      adapter.go            Telegram long-polling, voice/document/photo handling,
                            inline keyboard send/answer, SendDocument
      renderer.go           Markdown→HTML conversion, table rendering,
                            UTF-8-safe Split / Truncate
    feishu/
      adapter.go            Feishu WebSocket/webhook, interactive card send/patch,
                            token management, AES event decryption, SendDocument,
                            voice/file/image download
      renderer.go           Telegram HTML → Lark Markdown conversion, SplitText
      ws.go                 Feishu WebSocket long-connection protocol (protobuf)
    qqbot/
      adapter.go            QQ group bot WebSocket (outbound), JSON opcode protocol,
                            token from bots.qq.com, delayed-send handle
    dingtalk/
      adapter.go            DingTalk stream mode WebSocket (outbound), EVENT/SYSTEM
                            frames, sessionWebhook reply, delayed-send handle
    wework/
      adapter.go            WeCom HTTP webhook server, AES-256-CBC decryption,
                            SHA1 signature verify, REST API reply, delayed-send handle

  transcribe/
    transcribe.go           Transcriber interface, Config, New() factory
    groq.go                 Groq cloud Whisper API
    whisper_cpp.go          whisper.cpp CLI subprocess + ffmpeg conversion
    whisper_py.go           openai-whisper Python CLI subprocess
    whisper_cgo.go          whisper.cpp CGo bindings  (build tag: whisper)
    whisper_cgo_stub.go     no-op stub for standard builds

  lazycoding/
    lazycoding.go           orchestration: dispatch, queue, consumeStream,
                            handleCommand, handleCallback,
                            handleDownload, handleLS, handleTree, handleCat
    convlog.go              human-readable conversation transcript (verbose mode)

config.example.yaml         annotated configuration template
```

---

## Key Interfaces

### `channel.Channel` — platform abstraction

```go
type Channel interface {
    Events(ctx context.Context) <-chan InboundEvent
    SendText(ctx, conversationID, text string) (MessageHandle, error)
    UpdateText(ctx context.Context, handle MessageHandle, text string) error
    SendTyping(ctx context.Context, conversationID string) error
    SendKeyboard(ctx, conversationID, text string,
                 buttons [][]KeyboardButton) (MessageHandle, error)
    AnswerCallback(ctx context.Context, callbackID, notification string) error
    SendDocument(ctx, conversationID, filePath, caption string) error
}
```

`InboundEvent` carries all inbound traffic (text, voice, files, commands, inline button presses) in a single unified struct:

| Field            | Description |
|------------------|-------------|
| `UserKey`        | `"tg:{userID}"` / `"fs:{openID}"` / `"qq:{memberOpenID}"` / `"dt:{staffID}"` / `"ww:{userID}"` — sender identity |
| `ConversationID` | Telegram chat ID string — which project context to use |
| `Text`           | message text; for voice messages: the transcription |
| `IsCommand`      | true when the message starts with `/` |
| `Command`        | command name without `/`, e.g. `"reset"` |
| `CommandArgs`    | text after the command name |
| `IsVoice`        | true when text was obtained via speech-to-text |
| `IsCallback`     | true for inline keyboard button presses |
| `CallbackID`     | must be acknowledged with `AnswerCallback` |
| `CallbackData`   | application-defined payload (e.g. `"cancel"`, `"yes"`) |

`MessageHandle.Seal()` marks a message as final, preventing further edits (used after streaming completes or the task is cancelled).

---

### `agent.Agent` — AI backend abstraction

```go
type Agent interface {
    Stream(ctx context.Context, req StreamRequest) (<-chan Event, error)
}
```

`StreamRequest`:

| Field        | Description |
|--------------|-------------|
| `Prompt`     | user instruction |
| `SessionID`  | resumes an existing Claude session; empty = new session |
| `WorkDir`    | Claude's working directory |
| `ExtraFlags` | additional CLI flags (e.g. `--model claude-opus-4-6`) |

`Event.Kind` values:

| Kind | Payload | When emitted |
|------|---------|--------------|
| `EventKindInit` | `SessionID` | First event; provides the session ID for the current run |
| `EventKindText` | `Text` | Incremental Claude text output |
| `EventKindToolUse` | `ToolName`, `ToolInput`, `ToolUseID` | Claude invoked a tool |
| `EventKindToolResult` | `ToolUseID`, `ToolResult` | Tool returned a result |
| `EventKindResult` | `SessionID`, `Text`, `Usage` | Final event; session ID may be updated; Usage carries token counts and cost |
| `EventKindError` | `Err` | Non-recoverable error (timeout, crash, etc.) |

---

### `session.Store` — persistence abstraction

```go
type Store interface {
    Get(key string) (Session, bool)
    Set(key string, s Session)
    Delete(key string)
}
```

Two implementations:
- **`MemoryStore`** — in-process, lost on restart (kept for testing/embedding)
- **`FileStore`** — JSON-backed, persists to `~/.lazycoding/sessions.json`; survives restarts

The production entry point (`cmd/lazycoding/main.go`) always uses `FileStore`.

---

### `transcribe.Transcriber` — speech-to-text abstraction

```go
type Transcriber interface {
    Transcribe(ctx context.Context, audioPath string) (string, error)
}
```

Four backends, selectable via config with no code changes. See [Voice input pipeline](#voice-input-pipeline) below.

---

## Per-Conversation Project Mapping

The session store and request-serialisation map are both keyed by **`sessionKey`**, which is the work directory path when one is configured, or the `ConversationID` otherwise (`sessionKey = workDir if non-empty, else convID`). Rationale:

- Multiple conversations that point at the same project directory share one Claude session and are serialised automatically.
- All members of a group share the same Claude session and see each other's progress.
- Conversations without a configured work directory fall back to chat ID isolation.

### Config resolution (waterfall)

```
channels["<chatID>"].work_dir    ← highest priority
channels["<chatID>"].extra_flags
        ↓
claude.work_dir                  ← global default
claude.extra_flags
        ↓
(lazycoding launch directory)    ← ultimate fallback
```

---

## Request Lifecycle

```
Telegram update arrives
  └─ Adapter.toEvent()            [per-update goroutine — non-blocking]
       ├─ command? → IsCommand = true
       ├─ voice?   → downloadFile → Transcribe(ctx, oggPath) → IsVoice = true
       ├─ document? → downloadFile → work_dir/filename
       ├─ photo?   → downloadFile → work_dir/photo_*.jpg
       └─ text?    → Text = msg.Text
            │
            ▼ → buffered channel (size 16)
  lazycoding.Run()                [single event-loop goroutine]
       ├─ IsCallback → go handleCallback()   ← inline button press
       ├─ IsCommand  → go handleCommand()    ← fast, no Claude subprocess
       └─ else       → dispatch(ev)
                           │
                           ├─ Claude already running for this sessionKey (workDir or convID)?
                           │      YES → append ev to queue
                           │          → SendText("⏳ Queued — will run after the current task.")
                           │            to conversation; return
                           │      NO  → startRequest(ev)
                           │
                           └─ startRequest()
                                  ├─ ctx, cancel = context.WithTimeout(900s)
                                  ├─ pending[sessionKey] = {cancel, done, queue}
                                  ├─ go typingKeepalive(convID, every 4s)
                                  └─ go handleMessage(ctx, ev)
                                          ├─ WorkDirFor / ExtraFlagsFor
                                          ├─ store.Get(sessionKey) → sessionID
                                          │    (fallback: discoverLocalSession(workDir))
                                          ├─ ag.Stream(ctx, req) → events chan
                                          ├─ SendKeyboard("⏳ thinking…",
                                          │    [[✕ Cancel]]) → handle
                                          └─ consumeStream(handle, events)
                                                  ├─ EventKindText    → throttled UpdateText
                                                  ├─ EventKindToolUse → update placeholder
                                                  ├─ EventKindToolResult → show output
                                                  ├─ EventKindResult  → final flush + Seal
                                                  │    if tools+text > 4096 chars:
                                                  │      update placeholder with tool summary
                                                  │      send full reply text as new message(s)
                                                  └─ EventKindError   → Seal + send error msg
                                                       if thinking-signature error:
                                                         prompt user to /reset
                                                       │
                                                       ▼ goroutine exits
                                               dequeue: if queue non-empty
                                                   → startRequest(queue[0])
```

---

## Streaming Update Strategy

The core UX challenge is mapping a streaming terminal session to a chat message. lazycoding uses **edit-in-place** with throttling:

```
1. Send placeholder message:  "⏳ thinking…"  [✕ Cancel]
2. As events arrive:
   ├─ Tool calls  → update placeholder with tool name + truncated input
   │               show output snippet when tool returns
   └─ Text chunks → accumulate in strings.Builder
                    UpdateText every edit_throttle_ms (default 1000 ms)
3. On EventKindResult:
   └─ Final UpdateText with full Markdown→HTML rendered response
      Seal the handle (no further edits)
4. If response ends with "?":
   └─ SendKeyboard with [✅ Yes] [❌ No] quick-reply buttons
```

| Event | Action |
|-------|--------|
| `EventKindInit` | Capture session ID |
| `EventKindText` | Append to buffer; `UpdateText` if throttle elapsed |
| `EventKindToolUse` | Replace placeholder with tool name + input |
| `EventKindToolResult` | Append truncated output under tool entry |
| `EventKindResult` | Final flush, `Seal`, optional quick-reply keyboard; accumulate `Usage` into session; if combined tool summary + reply text exceeds 4096 chars, update placeholder with tool summary and send the full reply text as new message(s) via `Split` |
| `EventKindError` | `Seal` + send `⚠️ Error:` message; if the error is an expired thinking-block signature, prompt the user to run `/reset` |

**Message size limits:** Telegram caps messages at 4096 bytes. `Split` breaks large responses into multiple messages; `UpdateText` uses `Truncate`. Both functions respect UTF-8 rune boundaries (never cut a multi-byte character in half). When `EventKindResult` arrives and the combined content (tool summary + reply text) exceeds 4096 characters, the tool summary is kept in the original placeholder message and the full reply text is sent as one or more new messages using `Split`, ensuring no content is ever truncated.

### Tool Input Formatting

`formatToolInput(toolName, input, workDir string) string` (defined in `convlog.go`, used by both the terminal verbose log and the Telegram message builder) extracts a human-readable summary from the raw JSON input of each tool call:

| Tool | Displayed as |
|------|-------------|
| `Read` / `Write` / `Edit` | File path relative to `workDir`; if still > 80 chars, last 3 segments with `…/` prefix |
| `Bash` | Full command string (up to 200 chars) |
| `Glob` | Pattern + shortened directory |
| `Grep` | Pattern + optional glob filter + shortened path |
| `WebFetch` | URL (up to 120 chars) |
| `WebSearch` | Query string |
| `Task` | Description (up to 120 chars) |
| `AskUserQuestion` | First question text (up to 120 chars) |
| `TodoWrite` | `(N todos)` |
| others | Truncated raw JSON (up to 160 chars) |

**HTML rendering:** All text from Claude is passed through `MarkdownToTelegramHTML`, which converts fenced code blocks, tables (Unicode box-drawing), headers, bold/italic/strikethrough, inline code, blockquotes, links, and bullet lists into Telegram's HTML parse mode. Raw HTML entities are escaped using only the four named entities Telegram accepts (`&amp;` `&lt;` `&gt;` `&quot;`).

---

## Claude CLI Invocation

```sh
claude \
  --print \
  --output-format stream-json \
  --dangerously-skip-permissions \
  [--resume <session_id>] \
  [extra_flags...] \
  "<prompt>"
```

- `stream-json` emits one JSON object per line on stdout.
- `parser.ParseLineMulti` converts each line to zero or more `agent.Event` values, handling multi-block assistant turns (text + tool_use in a single assistant message).
- `exec.CommandContext` guarantees SIGKILL when the context is cancelled (timeout or `/cancel`).
- stderr is captured; non-empty stderr is appended to the error message surfaced to the user.
- Scanner buffer is 4 MB to handle large tool outputs without truncation at the parser layer.

---

## Voice Input Pipeline

```
Telegram sends OGG/OPUS voice message
  └─ handleVoice()
       ├─ downloadFile(fileID) → /tmp/lc-voice-<nano>.ogg
       └─ transcriber.Transcribe(ctx, oggPath) → text
            │
            ├─ backend="groq"
            │    └─ multipart POST to api.groq.com/v1/audio/transcriptions
            │       (OGG accepted natively; no conversion needed)
            │
            ├─ backend="whisper-native"
            │    └─ ffmpeg OGG→16kHz mono WAV
            │       → whisper.cpp CGo bindings → []float32 samples → text
            │       (model auto-downloaded from HuggingFace on first use)
            │
            ├─ backend="whisper-cpp"
            │    └─ [ffmpeg OGG→WAV if available]
            │       → exec whisper-cli subprocess → parse .txt output
            │
            └─ backend="whisper"
                 └─ exec whisper Python subprocess → parse .txt output
```

The transcribed text becomes `InboundEvent.Text` with `IsVoice=true`. The orchestration layer echoes it back (`🎤 Transcribed: …`) before forwarding to Claude, letting the user confirm what was recognised.

| Backend | Install | OGG support | Notes |
|---------|---------|-------------|-------|
| `groq` | API key only | native | Recommended; 28,800 s/day free |
| `whisper-native` | `brew install whisper-cpp` | via ffmpeg | CGo; `-tags whisper` build required |
| `whisper-cpp` | `brew install whisper-cpp` | via ffmpeg | CLI subprocess |
| `whisper` | `pip install openai-whisper` | native | Python subprocess |

---

## File Upload Pipeline

Both adapters save uploaded files to the configured `work_dir` and notify Claude via `InboundEvent.Text`.

**Telegram:**
```
document / photo message
  └─ handleDocument() / handlePhoto()
       ├─ workDir = cfg.WorkDirFor(convID)
       ├─ sanitizeFilename() — strip directory components (path traversal prevention)
       ├─ downloadFile(fileID) → workDir/<filename>
       └─ InboundEvent{Text: "[File saved to work directory: <name>]\n<caption>"}
```

**Feishu:**
```
file message  → handleFile()
                  ├─ parse content {"file_key","file_name"} from Content JSON
                  ├─ downloadResource(messageID, file_key, "file") → workDir/<filename>
                  └─ InboundEvent{Text: "[File saved to work directory: <name>]"}

image message → handleImage()
                  ├─ parse content {"image_key"} from Content JSON
                  ├─ downloadResource(messageID, image_key, "image") → workDir/photo_*.jpg
                  └─ InboundEvent{Text: "[File saved to work directory: <name>]"}

audio message → handleAudio()  (same pipeline as Telegram voice)
                  ├─ parse content {"file_key"} from Content JSON
                  ├─ downloadResource(messageID, file_key, "file") → /tmp/lc-feishu-voice-*.ogg
                  ├─ tr.Transcribe(ctx, tmpFile) → text
                  └─ InboundEvent{Text: text, IsVoice: true}
```

`downloadResource` calls `GET /im/v1/messages/{message_id}/resources/{key}?type={file|image}` with a valid tenant token and streams the response directly to disk.

The event text is the prompt sent to Claude — it tells Claude exactly where the file landed so it can act on it without any additional instruction.

---

## Filesystem Commands

Three commands execute shell-style filesystem operations **directly in lazycoding**, without spawning a Claude subprocess. All paths go through `safeJoin` (must stay within `workDir`).

### `/ls [path]`

```
/ls src/
  └─ safeJoin(workDir, "src/")
       └─ os.ReadDir(target)
            └─ format each entry: mode  size  mtime  name/
                 mode   = info.Mode().String()     e.g. "-rw-r--r--"
                 size   = formatFileSize(n)        e.g. "1.2K", "4.0M"
                 mtime  = ModTime().Format("Jan 02 15:04")
                 name   = entry name (trailing "/" for dirs)
            └─ SendText("<pre>…</pre>")
```

### `/tree [path]`

```
/tree
  └─ walk(workDir, prefix="", depth=0)
       ├─ max depth:   3
       ├─ max entries: 150
       ├─ skip dirs:   .git, node_modules, vendor, .cache, __pycache__, .next
       └─ SendText("<pre>…</pre>")
```

### `/cat <path>`

```
/cat src/main.go
  └─ safeJoin(workDir, "src/main.go")
       └─ os.ReadFile(absPath)
            ├─ truncate at 200 lines or 8000 bytes (whichever comes first)
            └─ SendText("<code>path</code>\n<pre>…</pre>[(truncated)])
```

## File Download Pipeline

```
/download src/main.go
  └─ safeJoin(workDir, "src/main.go")
       ├─ filepath.Clean(filepath.Join(workDir, rel))
       ├─ verify result has workDir as prefix (rejects ../../ traversal)
       └─ ch.SendDocument(ctx, convID, absPath, rel)
```

---

## Concurrency Model

```
┌─ 1 polling goroutine (Adapter.Events loop) ─────────────────────┐
│   per-update goroutines (download + transcribe — non-blocking)  │
│   → buffered channel (size 16)                                  │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─ 1 event-loop goroutine (lazycoding.Run) ────────────────────────┐
│   reads Events() sequentially                                   │
│   callbacks  → go handleCallback()   (fast)                     │
│   commands   → go handleCommand()    (fast)                     │
│   messages   → dispatch()                                       │
│     ├─ busy? → append to queue (pendingState.queue, guarded by  │
│     │          pendingState.mu)                                  │
│     └─ idle? → startRequest() → go handleMessage()             │
│                  ├─ context.WithTimeout                         │
│                  └─ on exit: dequeue next item if any           │
└─────────────────────────────────────────────────────────────────┘

pending  map[sessionKey → *pendingState]  (sessionKey = workDir or convID)  guarded by pendingMu (outer lock)
pendingState.queue                        guarded by pendingState.mu (inner lock)
```

**Invariants:**
- At most **one** Claude subprocess runs per work directory (or conversation, when no work dir is configured) at any time.
- Messages are never dropped; they queue until Claude is ready.
- `cancelConversation()` cancels the subprocess *and* drains the queue atomically.
- All locks are short-held (no I/O inside critical sections).

---

## Channel Adapters

### Adapter comparison

| Aspect | Telegram | Feishu (default) | Feishu (webhook) | QQ Bot | DingTalk | WeCom |
|--------|----------|------------------|-----------------|--------|----------|-------|
| Event delivery | Long-polling | WebSocket (outbound) | HTTP webhook | WebSocket (outbound) | WebSocket (outbound) | HTTP webhook |
| Public IP | ❌ No | ❌ No | ✅ Yes | ❌ No | ❌ No | ✅ Yes |
| Message format | Telegram HTML | Lark Markdown card | Lark Markdown card | Plain text | Markdown | Markdown |
| Edit-in-place | `editMessageText` | PATCH card | PATCH card | ❌ Delayed-send | ❌ Delayed-send | ❌ Delayed-send |
| Token / auth | Static bot token | `tenant_access_token` (2h TTL) | same | `access_token` (bots.qq.com) | `accessToken` (2h TTL) | `access_token` (2h TTL) |
| Voice / file / image | ✅ Fully handled | ✅ Fully handled | ✅ Fully handled | ❌ | ❌ | ❌ |
| Inline keyboards | ✅ | ✅ (card actions) | ✅ | ❌ | ❌ | ❌ |

### Telegram

Long-polling via `telegram-bot-api`. Each update runs a goroutine for download/transcription. `tgHandle` tracks whether an inline keyboard is attached so `UpdateText` can strip it on first real content.

### Feishu / Lark

**WebSocket mode (default):** Calls `POST /callback/ws/endpoint` to get a `wss://` URL, then dials with `gorilla/websocket`. Frames are binary protobuf (hand-rolled encoder/decoder; no SDK). `method=0` = ping/pong, `method=1` = events. ACK sent per event frame (`{"code":200}`). Reconnects with exponential backoff (2s → 60s).

**Webhook mode:** Starts an HTTP server; Feishu pushes events. Optional AES-CBC-256 event encryption (`sha256(encrypt_key)` as AES-256-CBC key).

**Card format:** Interactive cards with `lark_md` div + optional `action` buttons. `UpdateText` → PATCH `/im/v1/messages/{id}` (edit-in-place streaming).

**Renderer:** `TelegramHTMLToLarkMarkdown` converts Telegram HTML → Lark Markdown. `SplitText` splits at newline boundaries (max 3000 runes per card).

### QQ Group Bot

**Protocol:** Outbound WebSocket to `wss://api.sgroup.qq.com/websocket`. JSON opcode handshake: OP10 Hello → OP2 Identify (intent=`GROUP_AND_C2C_EVENT`, 1<<25) → OP11 heartbeat ACK loop. Events arrive as OP0 Dispatch with type `GROUP_AT_MESSAGE_CREATE`.

**Token:** POST to `https://bots.qq.com/app/getAppAccessToken` with `{appId, clientSecret}`. TTL from `expires_in` field (string, seconds). Authorization header: `QQBot <token>`.

**Reply:** `POST /v2/groups/{group_openid}/messages` with `{content, msg_type:0, msg_id}`. The `msg_id` references the original user message (5-minute passive reply window). The adapter stores the latest `msg_id` per `group_openid` in a map.

**Delayed-send handle:** `SendText/SendKeyboard` sends an initial message immediately and returns a handle. `UpdateText` buffers. `Seal()` sends the final accumulated text via the REST API.

**Renderer:** `htmlToPlainText` — strips all HTML tags, unescapes entities. QQ plain text only.

### DingTalk Stream

**Protocol:** Calls `POST https://api.dingtalk.com/v1.0/gateway/connections/open` (with `x-acs-dingtalk-access-token`) to get a WebSocket endpoint URL. Connects to the URL; receives JSON stream frames with `type` (SYSTEM/EVENT) and `headers.topic`.

SYSTEM frames: `ping` → reply `pong` (same `messageId`). EVENT frames: ACK with `{"code":200}` SYSTEM frame, then dispatch.

Bot message events have `topic=/v1.0/im/bot/messages/getAll`. Each message includes a `sessionWebhook` URL for replying (valid ~2 hours).

**Reply:** POST to `sessionWebhook` with `{msgtype:"markdown", markdown:{title, text}}`. No auth token needed (webhook URL is self-authenticating).

**Delayed-send handle:** Same pattern as QQ. `Seal()` posts to the latest stored `sessionWebhook` for the conversation.

**Renderer:** `htmlToMarkdown` — same logic as Feishu's `TelegramHTMLToLarkMarkdown`.

### WeCom (企业微信)

**Protocol:** HTTP webhook server (same as Feishu webhook mode). GET = URL verification (decrypt `echostr`, return plaintext). POST = message callback (decrypt XML, dispatch).

**Decryption:** `EncodingAESKey` (43 base64 chars + "=" padding) → 32-byte AES key. IV = first 16 bytes of key. AES-256-CBC decrypt → PKCS7 unpad → strip 16-byte random prefix + 4-byte length field → XML content + corpID suffix.

**Signature:** `SHA1(sorted([token, timestamp, nonce, encrypt]))`.

**Reply:** GET access token from `/cgi-bin/gettoken?corpid=&corpsecret=`, then POST to `/cgi-bin/message/send` with `{touser, msgtype:"markdown", agentid, markdown:{content}}`.

**Delayed-send handle:** Same pattern as QQ/DingTalk. `ConversationID` = `FromUserName` (user's openid), used as `touser` in replies.

**Renderer:** `htmlToMarkdown` — same as DingTalk.

---

## Interactive Features

### Inline Cancel Button

The initial placeholder message includes an inline keyboard with a **[✕ Cancel]** button. When clicked:
1. Telegram sends a `CallbackQuery` update.
2. `handleCallback()` calls `AnswerCallback` (removes Telegram's loading spinner).
3. `cancelConversation(convID)` cancels the Claude subprocess context and drains the queue.
4. A "⏹ Cancelled." message is sent.

The `hasKeyboard` flag on `tgHandle` ensures the button is removed the first time `UpdateText` is called (real content replaces the placeholder).

### Quick-Reply Buttons

After `consumeStream` returns, `detectQuickReplies(finalText)` inspects the last non-empty line. If it ends with `?`, a `[✅ Yes]` / `[❌ No]` keyboard is sent as a separate message. Clicking a button dispatches the button's `Data` string (`"yes"` or `"no"`) as a new text message, which joins the queue and is processed normally.

### Typing Keepalive

A background goroutine sends `SendTyping` to the conversation every 4 seconds for the duration of the Claude request. Telegram's "typing…" indicator normally disappears after 5 seconds; the keepalive ensures it stays visible throughout long-running tasks, giving the user a clear signal that the bot is still working.

### Queue Notification

When a message arrives while Claude is still running, it is queued and the sender immediately receives `⏳ Queued — will run after the current task.` This ensures users are not left wondering whether their message was received.

### `/status` Query

`runningStatus` is a `sync.Map` (key = `sessionKey`, value = current rendered HTML) stored on the `Lazycoding` struct. It is written at two points:
1. **Immediately** when `consumeStream` starts — set to `"(thinking…)"` before any events arrive.
2. **After every `doFlush`** — updated to the latest rendered content (tool list + accumulated text).

It is deleted via `defer` when `consumeStream` returns. The `/status` command handler reads this map and sends the snapshot as a new chat message, giving the user a mid-task progress view without affecting the ongoing placeholder message.

---

## Session Persistence

```
Session{
    ClaudeSessionID   string    // passed as --resume <id> to claude CLI
    LastUsed          time.Time
    ModelOverride     string    // optional --model override for this session
    TotalCostUSD      float64   // accumulated cost across all turns
    TotalInputTokens  int       // accumulated input token count
    TotalOutputTokens int       // accumulated output token count
}
```

`FileStore` serialises the session map to `~/.lazycoding/sessions.json` on every `Set` or `Delete` call (write-through). On startup, `NewFileStore` reads the file back; a corrupt or missing file starts with an empty map (no crash).

This means:
- **Restart lazycoding** → sessions reload → Claude context is preserved.
- **`/reset`** → `store.Delete(sessionKey)` → Claude starts a fresh session.
- **`/resume <id>`** → `store.Set(sessionKey, session{ClaudeSessionID: id})` → Claude resumes the specified session on the next request (preserves other session fields such as `ModelOverride`).
- **Session file manually deleted** → all conversations start fresh (no harm done).
- **`/model <name>`** → `session.ModelOverride = name` → applied as `--model` flag on every subsequent request (replaces any existing `--model` in config `extra_flags`)
- **`/cost`** → reads `TotalCostUSD`, `TotalInputTokens`, `TotalOutputTokens` from session
- Session save is now **read-modify-write** (preserves all fields including `ModelOverride` and usage counters across turns)

### Session Key

Sessions are keyed by **work directory path** (when configured) rather than conversation ID. `sessionKey()` returns `workDir` if non-empty, otherwise `convID`. Consequences:

- Multiple Telegram conversations pointing at the same directory share one Claude session.
- Requests for the same directory are serialised (at most one Claude subprocess per directory).
- `/reset` and `/session` commands operate on the shared session key.

### Local Session Discovery

When no stored session exists for a work directory, `discoverLocalSession(workDir)` scans `~/.claude/projects/<encoded>/` for `.jsonl` files and returns the most recently modified filename (without `.jsonl`) as the session ID to resume.

Claude Code encodes the project path by replacing every `/` with `-`, so `/Users/hua/projects/foo` maps to `~/.claude/projects/-Users-hua-projects-foo/`.

Since Claude Code stores all sessions (interactive and `--print` alike) in the same per-project directory, this allows lazycoding to transparently continue a session started in the local CLI, and vice versa.

If lazycoding already has a stored session, it takes priority (the discovered local session is ignored until `/reset` is run).

---

## Adding a New Conversation

1. Add the bot to the target Telegram chat.
2. Send `/workdir` — the terminal log shows `conversation=<chatID>`.
3. Edit `config.yaml`:
   ```yaml
   channels:
     "<chatID>":
       work_dir: "/path/to/project"
   ```
4. Restart lazycoding. No code changes required.

---

## Extending to Other Platforms

Implement `channel.Channel` for Slack, Discord, or any other messaging platform. The core orchestration layer, agent runner, session store, and transcription layer are all platform-agnostic. Wire the new adapter in `cmd/lazycoding/main.go`.

**Built-in adapters:**
- **Telegram** (`internal/channel/telegram`) — long-polling, voice, file upload/download, inline keyboards, edit-in-place streaming
- **Feishu/Lark** (`internal/channel/feishu`) — WebSocket + webhook, interactive cards, AES event decryption, voice/file/image, edit-in-place via card PATCH
- **QQ Group Bot** (`internal/channel/qqbot`) — outbound WebSocket, JSON opcodes, delayed-send handle
- **DingTalk** (`internal/channel/dingtalk`) — stream mode WebSocket, sessionWebhook reply, delayed-send handle
- **WeCom** (`internal/channel/wework`) — HTTP webhook, AES-CBC decryption, REST API reply, delayed-send handle

**Adapter selection** in `cmd/lazycoding/main.go` — all platforms can be active simultaneously:
```go
var adapters []channel.Channel
if cfg.Feishu.AppID != ""    { adapters = append(adapters, fsadapter.New(cfg, tr)) }
if cfg.Telegram.Token != ""  { adapters = append(adapters, tgadapter.New(cfg, tr)) }
if cfg.QQBot.AppID != ""     { adapters = append(adapters, qqadapter.New(cfg)) }
if cfg.DingTalk.AppKey != "" { adapters = append(adapters, dtadapter.New(cfg)) }
if cfg.WeWork.CorpID != ""   { adapters = append(adapters, wwadapter.New(cfg)) }
ch := channel.NewMultiAdapter(adapters...)  // fan-in + routing
```

`channel.NewMultiAdapter` wraps multiple adapters behind a single `Channel` interface.  Events from all adapters are fanned into one channel; outbound calls (`SendText`, `UpdateText`, …) are routed back to the originating adapter via a `conversationID → adapter` map populated as events arrive. `MessageHandle` values are wrapped in a `multiHandle` that preserves the originating adapter for `UpdateText` routing.

**Adding a new adapter** (e.g. Slack):
```go
slackCh, _ := slack.New(cfg)
b := lazycoding.New(slackCh, runner, store, cfg)
b.Run(ctx)
```

---

## Design Decisions and Trade-offs

### `--print` batch mode vs PTY

Claude Code has an interactive PTY mode and a `--print` batch mode. lazycoding uses `--print --output-format stream-json` because:

- **Structured output** — `stream-json` emits machine-readable events (text, tool_use, tool_result, result) that can be parsed and rendered selectively.
- **Clean subprocess management** — `exec.CommandContext` with SIGKILL on cancel is reliable; PTY lifecycle is more complex.
- **Message queuing** — `--print` processes one request at a time, which pairs naturally with a per-conversation FIFO queue.

The trade-off: Claude cannot ask interactive clarifying questions mid-task. The quick-reply button feature partially compensates for this at the chat layer.

### Edit-in-place vs new messages

Telegram supports editing existing messages within a ~48-hour window. Edit-in-place keeps the conversation thread clean (one message per Claude turn) and provides a natural "streaming" feel on mobile. The throttle (`edit_throttle_ms`, default 1000 ms) prevents 429 rate-limit errors.

For large responses where the combined tool summary and reply text exceed 4096 bytes, the tool summary is kept in the original placeholder message and the full reply text is sent as one or more new messages via `Split`.

### Session key = work directory path (with ConversationID fallback)

`sessionKey()` returns the configured work directory path when one is set, and falls back to the conversation ID otherwise. Consequences:

- Multiple Telegram conversations (for example, your phone and your desktop) that point at the same project directory automatically share one Claude session and have their requests serialised — no duplicate work, no race conditions.
- This mirrors how a team shares a codebase: the project directory is the natural unit of collaboration, not a particular chat window.
- When no work directory is configured, the fallback to `convID` preserves the original per-conversation isolation.
- If per-user isolation were needed inside a shared project (e.g., each developer works on their own branch), the key could be changed to `UserKey` with minimal code changes.
