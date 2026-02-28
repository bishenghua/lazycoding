# lazycoding – Architecture & Design

## Overview

lazycoding is a local gateway that bridges Telegram chats to the `claude` CLI.
Each Telegram chat (private, group, or channel) maps to one Claude *working
directory*, i.e. one project.  A single bot process manages multiple projects
simultaneously.

```
Telegram (phone/desktop)
        │  send message / voice / file
        ▼
  lazycoding  (runs locally)
        │  spawn subprocess
        ▼
  claude CLI  (--dangerously-skip-permissions)
        │  reads/writes files, runs commands
        ▼
  stream-json output  →  throttled edit-in-place back to Telegram
```

---

## Directory Structure

```
cmd/lazycoding/
  main.go                     entry point: wire deps, graceful shutdown

internal/
  config/
    config.go                 Config structs, YAML loading, defaults,
                              WorkDirFor / ExtraFlagsFor helpers

  agent/
    agent.go                  Agent interface, StreamRequest, Event types
    claude/
      runner.go               spawn claude subprocess, set WorkDir
      parser.go               stream-json JSONL → []agent.Event

  session/
    store.go                  Store interface + MemoryStore

  channel/
    channel.go                Channel interface, InboundEvent, MessageHandle
    telegram/
      adapter.go              Telegram implementation: polling, voice,
                              document/photo upload, SendDocument
      renderer.go             Split / Truncate for 4096-char limit

  transcribe/
    transcribe.go             Transcriber interface, Config, New() factory
    groq.go                   Groq cloud API backend
    whisper_cpp.go            whisper.cpp CLI backend
    whisper_py.go             openai-whisper Python CLI backend
    whisper_cgo.go            whisper.cpp CGo backend  (build tag: whisper)
    whisper_cgo_stub.go       stub for non-whisper builds

  lazycoding/
    lazycoding.go             orchestration: dispatch, consumeStream,
                              handleCommand, handleDownload, safeJoin

config.yaml                   annotated configuration template
DESIGN.md                     this file
README.md                     user-facing setup guide
```

---

## Key Interfaces

### `channel.Channel`  (platform abstraction)

```go
Events(ctx)                              → <-chan InboundEvent
SendText(ctx, conversationID, text)      → (MessageHandle, error)
UpdateText(ctx, handle, text)            → error
SendTyping(ctx, conversationID)          → error
SendDocument(ctx, conversationID, path,
             caption)                    → error
```

`InboundEvent` fields:

| Field            | Description                                          |
|------------------|------------------------------------------------------|
| `UserKey`        | `"tg:{userID}"` — who sent the message               |
| `ConversationID` | Telegram chat ID string — which chat                 |
| `Text`           | message text; for voice: the transcription           |
| `IsCommand`      | true when the message starts with `/`                |
| `Command`        | command name without `/`, e.g. `"reset"`             |
| `CommandArgs`    | text after the command                               |
| `IsVoice`        | true when text was transcribed from a voice message  |

### `agent.Agent`  (AI backend abstraction)

```go
Stream(ctx, StreamRequest) → (<-chan Event, error)
```

`StreamRequest`:

| Field        | Meaning                                              |
|--------------|------------------------------------------------------|
| `Prompt`     | user text                                            |
| `SessionID`  | resume a Claude session; empty = new session         |
| `WorkDir`    | claude working directory; empty = runner default     |
| `ExtraFlags` | additional CLI flags; nil = use runner default       |

`Event.Kind` values: `EventKindInit`, `EventKindText`, `EventKindToolUse`,
`EventKindResult`, `EventKindError`.

### `session.Store`  (persistence abstraction)

```go
Get(key string)    → (Session, bool)
Set(key string, s Session)
Delete(key string)
```

### `transcribe.Transcriber`  (speech-to-text abstraction)

```go
Transcribe(ctx, audioPath string) → (string, error)
```

---

## Per-Channel Project Mapping

### Session key = `ConversationID`

Both the session store and the request-serialisation map are keyed by the
**Telegram chat ID** (not the user ID).  Rationale:

- Each chat is configured to point at one project directory.
- All participants of a group share the same Claude context.
- Private chats are naturally isolated.

### Config resolution (waterfall)

```
channels["<chatID>"].work_dir     ← highest priority
claude.work_dir                   ← global default
(lazycoding launch directory)            ← ultimate fallback
```

Same waterfall applies to `extra_flags`.

### Example

```yaml
channels:
  "123456789":          # private chat → Project A
    work_dir: "/projects/project-a"

  "-1001234567890":     # group chat → Project B
    work_dir: "/projects/project-b"
    extra_flags: ["--model", "claude-opus-4-6"]
```

chat_id signs: private chats are positive; groups/channels are negative
(usually prefixed with `-100`).  Use `/workdir` in any chat to confirm.

---

## Request Lifecycle

```
Telegram update
  └─ Adapter.toEvent()            [per-update goroutine]
       ├─ command?  → base.IsCommand = true
       ├─ voice?    → downloadFile → Transcribe → base.IsVoice = true
       ├─ document? → downloadFile → workDir/filename
       ├─ photo?    → downloadFile → workDir/photo_*.jpg
       └─ text?     → base.Text
            │
            ▼ sent to out channel
  lazycoding.Run() [event-loop goroutine]
       ├─ IsCommand → go handleCommand()   (fast, no Claude)
       └─ else      → dispatch(ev)
                          ├─ cancel previous request for convID (SIGKILL)
                          ├─ <-old.done  (μs, OS reaps child)
                          └─ go handleMessage(ctx, ev)
                                  ├─ WorkDirFor / ExtraFlagsFor
                                  ├─ store.Get(convID)  → sessionID
                                  ├─ ag.Stream(ctx, req)  → subprocess
                                  ├─ SendText("_(thinking…)_") → handle
                                  └─ consumeStream()
                                        ├─ EventKindText    → throttled UpdateText
                                        ├─ EventKindToolUse → new status message
                                        ├─ EventKindResult  → final flush + Seal
                                        └─ store.Set(convID, {newSessionID})
```

---

## Streaming Update Strategy

| Event              | Action                                                        |
|--------------------|---------------------------------------------------------------|
| `EventKindInit`    | capture session ID                                            |
| `EventKindText`    | append to builder; `UpdateText` if ≥ `edit_throttle_ms`      |
| `EventKindToolUse` | send new `_Running: Tool: input_` status message             |
| `EventKindResult`  | final `UpdateText` + `Seal`; update tool handles → `_done_`  |
| `EventKindError`   | send `⚠️ Error:` message; seal handle if partial text exists  |

`edit_throttle_ms` (default 2000 ms) avoids Telegram's ~20 edits/min limit.
Messages > 4096 bytes are split (`Send` new chunk) or truncated (`UpdateText`).

---

## claude CLI Invocation

```
claude \
  --print \
  --output-format stream-json \
  --dangerously-skip-permissions \
  [--resume <session_id>] \
  [extra_flags...] \
  "<prompt>"
```

- `stream-json` emits one JSON object per line.
- `ParseLineMulti` converts each line to zero or more `agent.Event` values,
  handling multi-block assistant messages (e.g. text + tool_use).
- `exec.CommandContext` guarantees SIGKILL when context is cancelled.
- stderr is captured; non-empty stderr is appended to the error message.
- Scanner buffer is 4 MB to handle large tool outputs.

---

## Voice Input Pipeline

```
Telegram voice update (OGG/OPUS)
  └─ Adapter.handleVoice()
       ├─ downloadFile(voice.FileID) → /tmp/lc-voice-*.ogg
       └─ transcriber.Transcribe(ctx, oggPath) → text
            │
            ├─ backend="groq"           → HTTP multipart POST to Groq API
            ├─ backend="whisper-native" → ffmpeg OGG→WAV → whisper.cpp CGo
            ├─ backend="whisper-cpp"    → [ffmpeg OGG→WAV] → whisper-cli subprocess
            └─ backend="whisper"        → whisper Python subprocess
```

The transcribed text becomes `InboundEvent.Text` with `IsVoice=true`.  The lazycoding
layer echoes it back (`🎤 识别文字：…`) before forwarding to Claude.

### Transcription Backends

| Backend           | Install                        | OGG support | Notes                         |
|-------------------|--------------------------------|-------------|-------------------------------|
| `groq`            | none (API key only)            | native      | Recommended; free 28800 s/day |
| `whisper-native`  | `brew install whisper-cpp`     | via ffmpeg  | CGo; model auto-downloaded    |
| `whisper-cpp`     | `brew install whisper-cpp`     | via ffmpeg  | CLI subprocess                |
| `whisper`         | `pip install openai-whisper`   | native      | Python subprocess             |

`whisper-native` requires `go build -tags whisper`; all others use the
standard `go build`.

---

## File Upload Pipeline

```
Telegram document / photo update
  └─ handleDocument / handlePhoto
       ├─ workDir = cfg.WorkDirFor(convID)
       ├─ downloadFile(fileID) → workDir/<filename>
       └─ InboundEvent{Text: "[文件已保存: <name>]\n<caption>"}
```

- Document filename is sanitised (`filepath.Base`, strip leading dots).
- Photo is named `photo_YYYYMMDD_HHMMSS.jpg` (largest resolution chosen).
- The event text tells Claude where the file landed.

---

## File Download (`/download`)

```
/download src/main.go
  └─ safeJoin(workDir, "src/main.go") → absolute path
       ├─ path traversal check (must stay inside workDir)
       └─ ch.SendDocument(ctx, convID, absPath, rel)
```

`safeJoin` uses `filepath.Clean` + `strings.HasPrefix` to prevent
`../../etc/passwd`-style traversal.

---

## Concurrency Model

```
1 polling goroutine  (Adapter.Events)
  per-update goroutines  (file download / transcription, non-blocking)
    → buffered channel (size 16) → lazycoding.Run()

1 event-loop goroutine  (lazycoding.Run)
  reads Events() sequentially
  commands → go handleCommand()  (fast, no Claude)
  messages → dispatch()          (may block ~μs for subprocess death)
    → go handleMessage()  (at most 1 per active conversation)

pending map[convID → {cancel, done}]  guarded by pendingMu
```

Invariant: at most **one** Claude subprocess runs per conversation at any time.

---

## Commands

| Command               | Handler           | Notes                              |
|-----------------------|-------------------|------------------------------------|
| `/start`              | `handleCommand`   | welcome + current work_dir         |
| `/reset`              | `handleCommand`   | delete session + cancel in-flight  |
| `/session`            | `handleCommand`   | show Claude session ID             |
| `/workdir`            | `handleCommand`   | show active work_dir               |
| `/download <path>`    | `handleDownload`  | send file from work_dir to chat    |
| `/help`               | `handleCommand`   | command list                       |

---

## Adding a New Chat

1. Add the bot to the target chat.
2. Send `/workdir` — bot replies with current directory; terminal log shows
   `conversation=<chatID>`.
3. Add to `config.yaml`:
   ```yaml
   channels:
     "<chatID>":
       work_dir: "/path/to/project"
   ```
4. Restart: `./lazycoding config.yaml`.  No code changes required.

---

## Extending to Other Platforms

Implement `channel.Channel` for Slack, Discord, etc.  The bot core, agent
layer, session store, and transcription layer are all platform-agnostic.
Wire the new adapter in `cmd/lazycoding/main.go`.
