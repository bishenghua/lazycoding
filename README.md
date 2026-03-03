# lazycoding 🛋️

[English](../README.md) · [简体中文](docs/README.zh-CN.md)

**Control a local Claude Code session from your phone — over Telegram, Feishu (Lark), QQ, DingTalk, or WeCom.**

Write code, fix bugs, and manage multiple projects — all by sending a message. lazycoding runs on your machine, bridges your chat platform to the `claude` CLI, and streams every tool call and response back to your chat in real time.

```
You (anywhere, any device)
        │  "refactor the auth module and add tests"
        ▼
   Telegram / Feishu / QQ / DingTalk / WeCom
        │
        ▼
   lazycoding  ← runs on your dev machine
        │  claude --print --output-format stream-json
        ▼
   Claude Code  ← reads files, runs commands, writes code
        │
        ▼
   Streaming output → back to your chat in real time
```

---

## Why lazycoding?

**Claude Code is powerful but tied to a terminal.** lazycoding removes that constraint.

| Without lazycoding | With lazycoding |
|--------------------|-----------------|
| Must be at your computer | Works from your phone, tablet, or any Telegram client |
| One project per session | One bot serves multiple projects simultaneously |
| Manual session management | Sessions persist across restarts; context is never lost |
| No voice input | Dictate tasks hands-free |
| Files stay local | Send/receive files directly in chat |

**Ideal for:**
- Reviewing a PR on your phone and having Claude apply the fixes
- Dictating a feature spec during a commute and watching Claude implement it
- Sharing a Claude-powered coding assistant with your team over a group chat
- Running long tasks in the background and getting notified when they complete

---

## Table of contents

- [Prerequisites](#prerequisites)
- [Quick start](#quick-start)
- [Build](#build)
- [Step 1 – Create a Telegram bot](#step-1--create-a-telegram-bot)
- [Feishu setup](#feishu-setup)
- [Step 2 – Basic configuration](#step-2--basic-configuration)
- [Step 3 – Find your chat\_id](#step-3--find-your-chat_id)
- [Step 4 – Map conversations to projects](#step-4--map-conversations-to-projects)
- [Step 5 – Run](#step-5--run)
- [Commands](#commands)
- [Interactive features](#interactive-features)
- [Voice input](#voice-input)
- [File upload](#file-upload)
- [File download](#file-download)
- [Advanced configuration](#advanced-configuration)
- [FAQ](#faq)

---

## Prerequisites

| Dependency | Notes |
|------------|-------|
| Go 1.21+ | Build only; the compiled binary has no runtime dependencies |
| `claude` CLI | Must be logged in — `claude --version` should work |
| Telegram Bot Token | Obtain from @BotFather (free, takes 2 minutes) |
| Feishu Bot Credentials | Optional — App ID + App Secret from [open.feishu.cn](https://open.feishu.cn) |

Verify the Claude CLI works:

```bash
claude --version
claude --print "hello" --output-format stream-json --dangerously-skip-permissions
```

---

## Quick start

```bash
# 1. Clone and build
git clone https://github.com/bishenghua/lazycoding.git
cd lazycoding
make build

# 2. Create your config
cp config.example.yaml config.yaml
# Edit config.yaml: set telegram.token, allowed_user_ids, and claude.work_dir

# 3. Run
./lazycoding config.yaml

# 4. Open Telegram, send your bot a message — Claude starts working
```

---

## Build

```bash
# Standard build (recommended)
go build -o lazycoding ./cmd/lazycoding/

# With embedded whisper.cpp voice recognition (requires brew install whisper-cpp)
go build -tags whisper -o lazycoding ./cmd/lazycoding/

# Cross-compile for all platforms → dist/
make release
```

Available `make` targets:

```
make build          build for current platform
make build-whisper  build with CGo whisper voice recognition
make test           run tests
make release        cross-compile: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64
make clean          remove build artefacts
```

---

## Step 1 – Create a Telegram bot

1. Open Telegram → search **@BotFather** → send `/newbot`
2. Follow the prompts; BotFather gives you a token like `1234567890:ABCdef…`
3. Copy the token into `config.yaml` → `telegram.token`

---

## Step 2 – Basic configuration

```bash
cp config.example.yaml config.yaml
# Edit config.yaml
```

Minimum working config:

```yaml
telegram:
  token: "1234567890:ABCdefGHIjklMNOpqrsTUVwxyz"
  allowed_user_ids:
    - 123456789   # your Telegram user_id

claude:
  work_dir: "/Users/yourname/projects/my-project"
  timeout_sec: 900

log:
  format: "text"
  level: "info"
```

**Find your user\_id:** message **@userinfobot** on Telegram; the reply contains your `Id`.

---

## Step 3 – Find your chat\_id

Each conversation has a unique `chat_id`, used when mapping multiple projects.

### Using /workdir (recommended)

1. Start the bot: `./lazycoding config.yaml`
2. Send `/workdir` in the target conversation
3. Read the conversation id from the terminal log:
   ```
   level=INFO msg="request started" conversation=-1001234567890 ...
   ```

### chat\_id patterns

| Value | Conversation type |
|-------|------------------|
| Positive integer, e.g. `123456789` | Your DM with the bot |
| Negative integer, e.g. `-1001234567890` | Group / supergroup / channel |

> ⚠️ Negative chat\_ids **must be quoted** in YAML: `"-1001234567890":`

---

## Step 4 – Map conversations to projects

```yaml
channels:

  # Your DM with the bot → personal project
  "123456789":
    work_dir: "/Users/yourname/projects/personal"

  # Team group → shared backend project
  "-1001234567890":
    work_dir: "/Users/yourname/projects/backend-api"

  # Another group → different project, stronger model
  "-1009876543210":
    work_dir: "/Users/yourname/projects/ml-research"
    extra_flags:
      - "--model"
      - "claude-opus-4-6"
```

Unmapped conversations fall back to `claude.work_dir`.

---

## Step 5 – Run

```bash
./lazycoding config.yaml
```

For persistent background operation:

```bash
# tmux (recommended for development)
tmux new -s lazycoding
./lazycoding config.yaml

# nohup
nohup ./lazycoding config.yaml >> lazycoding.log 2>&1 &
```

**systemd service (Linux production):**

```ini
# /etc/systemd/system/lazycoding.service
[Unit]
Description=lazycoding Telegram–Claude gateway
After=network.target

[Service]
Type=simple
WorkingDirectory=/opt/lazycoding
ExecStart=/opt/lazycoding/lazycoding /opt/lazycoding/config.yaml
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
systemctl enable --now lazycoding
journalctl -fu lazycoding
```

---

## Feishu setup

Feishu defaults to **WebSocket long-connection mode** — the bot connects outbound to Feishu, so it works behind NAT/firewall with no public IP or port-forwarding needed (same as Telegram).

### Step 1 – Create a Feishu app

1. Go to [open.feishu.cn/app](https://open.feishu.cn/app) → **Create app** → choose **Custom app**
2. Note the **App ID** and **App Secret** from the **Credentials & Basic Info** tab
3. Under **Permissions** → add `im:message` (receive/send messages) and `im:message.group_at_msg` for group bots
4. Under **Event Subscriptions** → set the request URL to `http://<your-host>:8080/feishu`
   - Enable event: `im.message.receive_v1` (receive messages)
   - Enable event: `im.message.action.trigger_v1` (receive card button clicks)
5. Under **Bot** tab → enable the bot feature

### Step 2 – Configure lazycoding for Feishu

```yaml
feishu:
  app_id: "cli_xxxxxxxxxx"
  app_secret: "your-app-secret"
  # Default: WebSocket mode — no public IP needed (same as Telegram)
  # Set use_webhook: true only for server deployments with a public IP
  use_webhook: false

claude:
  work_dir: "/Users/yourname/projects/my-project"
  timeout_sec: 900

log:
  format: "text"
  level: "info"
```

### Step 3 – Run

```bash
./lazycoding config.yaml
# You'll see: "feishu ws: starting long connection (no public IP required)"
# "feishu ws: connected"
```

---

## QQ Bot setup

QQ group bots connect outbound via WebSocket — no public IP required.

1. Go to [bots.qq.com](https://bots.qq.com) → Create application
2. Enable **group bot** feature; note **AppID** and **Client Secret**
3. Configure intents: `GROUP_AND_C2C_EVENT` (1 << 25)
4. Add the bot to a QQ group; users `@mention` it to chat

```yaml
qqbot:
  app_id: "your-app-id"
  client_secret: "your-client-secret"
```

> **Note:** QQ bot replies cannot be edited after sending. lazycoding shows a "thinking…" message immediately, then sends the full response when Claude finishes.

---

## DingTalk setup

DingTalk stream mode connects outbound via WebSocket — no public IP required.

1. Go to [open.dingtalk.com](https://open.dingtalk.com) → Create **stream bot** application
2. Under **Event subscriptions** → enable **Stream mode**
3. Subscribe to topic: `/v1.0/im/bot/messages/getAll`
4. Copy **AppKey** and **AppSecret**
5. Add the bot to a DingTalk group; `@mention` it to chat

```yaml
dingtalk:
  app_key: "your-app-key"
  app_secret: "your-app-secret"
```

> **Note:** DingTalk replies cannot be edited. Same "send-on-done" pattern as QQ.

---

## WeCom (企业微信) setup

WeCom uses HTTP webhook callbacks — **requires a public IP or reverse proxy**.

1. Go to [work.weixin.qq.com](https://work.weixin.qq.com/wework_admin) → Applications → Create custom app
2. Note **CorpID**, **AgentID**, and **Agent Secret**
3. Under **Receive messages** → set URL to `http://<your-host>:8081/wework`
4. Note the **Token** and **EncodingAESKey**

```yaml
wework:
  corp_id: "your-corp-id"
  agent_id: 1000001
  agent_secret: "your-agent-secret"
  token: "your-token"
  encoding_aes_key: "your-43-char-base64-key"
  listen_addr: ":8081"
```

> **Note:** WeCom replies cannot be edited. Same "send-on-done" pattern as QQ/DingTalk.

---

### Platform comparison

| Feature | Telegram | Feishu | QQ Bot | DingTalk | WeCom |
|---------|----------|--------|--------|----------|-------|
| Public IP required | ❌ No | ❌ No | ❌ No | ❌ No | ✅ Yes |
| Connection mode | Long-poll | WebSocket | WebSocket | WebSocket | Webhook |
| Voice input | ✅ | ✅ | ✅ | ✅ | ✅ |
| File upload → project dir | ✅ | ✅ | ✅ | ✅ | ✅ |
| Image upload → project dir | ✅ | ✅ | ✅ | ✅ | ✅ |
| Inline cancel button | ✅ | ✅ | ❌ | ❌ | ❌ |
| Quick-reply buttons | ✅ | ✅ | ❌ | ❌ | ❌ |
| Message queuing | ✅ | ✅ | ✅ | ✅ | ✅ |
| Edit-in-place streaming | ✅ | ✅ (card) | ❌ (send-on-done) | ❌ (send-on-done) | ❌ (send-on-done) |
| `/download` | ✅ | ✅ | ❌ | ❌ | ❌ |

---

## Commands

**Session commands:**

| Command | Description |
|---------|-------------|
| `/start` | Welcome message + current work directory |
| `/workdir` | Show the work directory bound to this conversation (also shows chat\_id) |
| `/session` | Show the current Claude session ID (for debugging) |
| `/resume <id>` | Resume a specific Claude session by ID |
| `/status` | Show what Claude is doing right now — tool calls and output so far |
| `/cancel` | Stop the current task — **session history is kept** |
| `/reset` | Stop current task + **clear session history**, start fresh |
| `/compact [instructions]` | Compress the session context to save space; optional focus hint |
| `/model [name]` | Show the current model, or switch to a different one (e.g. `claude-opus-4-6`) |
| `/cost` | Show cumulative token usage and estimated cost for this session |

**Filesystem commands** (run directly, no Claude invocation):

| Command | Description |
|---------|-------------|
| `/ls [path]` | List directory contents with mode, size, and modification time |
| `/tree [path]` | Show directory tree up to 3 levels deep (skips `.git`, `node_modules`, `vendor`) |
| `/cat <path>` | View file contents (up to 200 lines / 8 KB) |
| `/download <path>` | Download a file from the work directory to Telegram |

**Info commands:**

| Command | Description |
|---------|-------------|
| `/help` | Show command reference |

---

## Interactive features

### Inline cancel button

Every response starts with a `⏳ thinking…` placeholder and a **[✕ Cancel]** button. Click it at any point to abort — session history is preserved, the message queue is cleared.

```
Bot: ⏳ thinking…   [✕ Cancel]
     ↓ click
Bot: ⏹ Cancelled.
```

### Quick-reply buttons

When Claude's response ends with a question, **[✅ Yes]** / **[❌ No]** buttons appear automatically. One tap sends your reply.

```
Bot: Should I also update the unit tests?
     [✅ Yes]  [❌ No]

You: [tap Yes]
Bot: ⏳ thinking…
```

### Message queuing

Messages sent while Claude is busy are queued — nothing is dropped, nothing is cancelled automatically. Claude works through them in order.

```
You: Analyse this file
Bot: ⏳ thinking…

You: (Claude is still running) Also check the dependencies
Bot: ⏳ Queued — will run after the current task.

Bot: Analysis: …
Bot: ⏳ thinking…   ← starts the queued request
Bot: Dependencies: …
```

### Tool call display

As Claude works, each tool call is shown inline with a human-readable summary instead of raw JSON:

```
🔧 Read: src/payment/handler.go
🔧 Edit: src/payment/handler.go
🔧 Bash: go test ./...
🔧 AskUserQuestion: Should I also update the integration tests?
🔧 TodoWrite: (3 todos)
```

File paths are shown relative to the work directory. The same formatting is used in the terminal verbose log.

---

## Voice input

Send a voice message on **any supported platform**; the bot transcribes it and forwards the text to Claude.

| Platform | Voice input | Audio format | Notes |
|----------|------------|--------------|-------|
| Telegram | ✅ | OGG/Opus | — |
| Feishu | ✅ | OGG | — |
| QQ Bot | ✅ | Platform-dependent | — |
| DingTalk | ✅ | AMR | — |
| WeCom | ✅ | AMR | Falls back to WeCom's built-in recognition when transcription is disabled |

Configure one transcription backend and it works on all platforms:

| Option | Backend value | Prerequisite | Privacy |
|--------|---------------|--------------|---------|
| **A: Groq API** (recommended) | `groq` | Free API key | Cloud |
| B: whisper-native (CGo) | `whisper-native` | `brew install whisper-cpp` + `-tags whisper` | Local |
| C: whisper.cpp CLI | `whisper-cpp` | `brew install whisper-cpp` | Local |
| D: openai-whisper | `whisper` | `pip install openai-whisper` | Local |

### Option A: Groq API (recommended, zero install)

Register at [console.groq.com](https://console.groq.com) → API Keys → Create key, then:

```yaml
transcription:
  enabled: true
  backend: "groq"
  groq:
    api_key: "gsk_your_key_here"
    model: "whisper-large-v3-turbo"
    language: "en"   # leave empty for auto-detect
```

Free tier: **28,800 seconds/day** (~8 hours). Accepts OGG natively.

### Option B: whisper-native (embedded, no subprocess)

```bash
brew install whisper-cpp ffmpeg
go build -tags whisper -o lazycoding ./cmd/lazycoding/
```

```yaml
transcription:
  enabled: true
  backend: "whisper-native"
  whisper_native:
    model: "base"   # auto-downloaded to ~/.cache/lazycoding/whisper/
    language: "en"
```

### Option C: whisper.cpp CLI

```bash
brew install whisper-cpp
whisper-download-ggml-model base
```

```yaml
transcription:
  enabled: true
  backend: "whisper-cpp"
  whisper_cpp:
    bin: "whisper-cli"
    model: "/opt/homebrew/share/whisper-cpp/models/ggml-base.bin"
    language: "en"
```

### Model reference

| Model | Size | Speed | Best for |
|-------|------|-------|----------|
| `tiny` | 75 MB | very fast | Short phrases, English |
| `base` | 140 MB | fast | **Recommended starting point** |
| `small` | 460 MB | medium | Technical terminology |
| `medium` | 1.5 GB | slow | High accuracy |
| `large-v3-turbo` | 809 MB | medium | High accuracy + speed |

---

## File upload

Drop any file or photo into the chat on **any supported platform**. lazycoding downloads it to the work directory and tells Claude it's there.

```
You: [upload requirements.txt]
     caption: Write a Dockerfile for this

Bot: 🔧 Read: requirements.txt
     Here is the Dockerfile: …
```

- Caption becomes the Claude prompt; you can also upload silently and ask in a follow-up message
- Photos → `photo_YYYYMMDD_HHMMSS.jpg`
- Directory components in filenames are stripped (path traversal prevention)

| Platform | How files are transferred |
|----------|--------------------------|
| Telegram | Telegram Bot API file download |
| Feishu | Feishu resource download API |
| QQ Bot | CDN attachment URL (authenticated) |
| DingTalk | `downloadCode` → DingTalk file download API |
| WeCom | `MediaId` → WeCom media/get API |

---

## File download

Retrieve any file from the project directory:

```
/download src/main.go
/download dist/app.tar.gz
```

Paths are relative to the conversation's work directory.

```
You: Write a data-processing script, save as process.py
Bot: Done, created process.py

You: /download process.py
Bot: [sends process.py]
```

---

## Advanced configuration

### Multiple users sharing one bot

```yaml
telegram:
  allowed_user_ids:
    - 111111111   # you
    - 222222222   # colleague A
    - 333333333   # colleague B
```

Leave `allowed_user_ids` empty to allow everyone. One conversation = one Claude process; incoming messages queue automatically.

### Specify the Claude model

```yaml
# Global default
claude:
  extra_flags:
    - "--model"
    - "claude-sonnet-4-6"

# Per-conversation override
channels:
  "-1001234567890":
    work_dir: "/projects/important"
    extra_flags:
      - "--model"
      - "claude-opus-4-6"
```

### Timeout

```yaml
claude:
  timeout_sec: 900   # 15 min; increase for very long tasks
```

### Terminal conversation log

```yaml
log:
  verbose: true
```

Prints a real-time, human-readable transcript to stderr:

```
15:04:05 ▶ conv=123456789  user:7846572322
  Refactor the payment module

15:04:07   🔧 Read  {"file_path":"/projects/api/payment.go"}
15:04:09   🔧 Edit  {"file_path":"/projects/api/payment.go",...
15:04:15 ◀ CLAUDE
  Done. Extracted PaymentService into its own struct, added
  interface, updated 3 call sites.
```

### Persistent sessions

Claude session IDs survive bot restarts. They are saved to `~/.lazycoding/sessions.json` automatically — no configuration needed. After a restart, each conversation resumes from exactly where it left off.

Sessions are keyed by **work directory path**. Multiple Telegram conversations pointing at the same project (for example, your phone and your desktop client in separate chats) automatically share a single Claude session, and their requests are serialised so they never overlap.

When lazycoding has no stored session for a work directory, it automatically scans `~/.claude/projects/` for sessions left by the local Claude Code CLI and resumes the most recently used one. This lets you start a task in the terminal and seamlessly continue it from Telegram (or vice versa). If lazycoding already has a stored session for that directory, it takes priority — run `/reset` to clear it and let auto-discovery pick up the latest local session.

### JSON logging

```yaml
log:
  format: "json"
  level: "info"
```

---

## FAQ

**Q: No response after sending a message**
→ Check `allowed_user_ids` contains your user\_id (or leave it empty)
→ Check the terminal for error logs
→ Verify `claude` is in PATH: `which claude`

**Q: "Error starting Claude" reply**
```bash
cd /your/work_dir
claude --print "hello" --output-format stream-json --dangerously-skip-permissions
```

**Q: YAML parse error on a negative chat\_id**
→ Must be quoted: `"-1001234567890":` not `-1001234567890:`

**Q: Task timed out (signal: killed)**
→ Increase `claude.timeout_sec` in config.yaml. Default is 900 s (15 min).
→ The Claude session is still saved; send a follow-up like "continue" to resume.

**Q: Voice message says "Voice transcription is not enabled"**
→ Set `transcription.enabled: true` and configure a backend (Groq is easiest):
```yaml
transcription:
  enabled: true
  backend: "groq"
  groq:
    api_key: "gsk_..."
```

**Q: "command not found: whisper-cli"**
→ `brew install whisper-cpp` then confirm with `which whisper-cli`

**Q: whisper-cpp OGG format error**
→ `brew install ffmpeg` (auto-used for conversion)
→ Or switch to Groq backend (accepts OGG natively)

**Q: Where did my uploaded file go?**
→ Saved in the conversation's `work_dir`. Run `/workdir` to see the path.

**Q: /download says "File not found"**
→ Path is relative to `work_dir`:
```
work_dir:  /projects/myapp
file:      /projects/myapp/src/main.go
command:   /download src/main.go
```

**Q: whisper-native build fails**
→ `brew install whisper-cpp` then `go build -tags whisper ./cmd/lazycoding/`

**Q: Session lost after restart**
→ Sessions are stored in `~/.lazycoding/sessions.json` automatically and survive restarts. If the file is missing or corrupt, a fresh session is started. Sessions are also keyed by work directory, so multiple Telegram conversations pointing at the same project automatically share one Claude session.

**Q: Can I share a Claude session between the local CLI and Telegram?**
→ Yes. When lazycoding has no stored session for a work directory, it automatically discovers the most recently used session from `~/.claude/projects/` and resumes it. This means you can:
  - Work locally in the terminal → switch to Telegram and continue the same context
  - Work from Telegram → then `claude --resume <session-id>` locally (session ID visible via `/session`)

If lazycoding already has a stored session, that takes priority. Run `/reset` to clear it and let auto-discovery pick up the latest local session.

Note: do not use both simultaneously (local CLI + Telegram) for the same session; two concurrent invocations writing to the same session can produce unpredictable results.

**Q: How do I go back to a previous session after /reset?**
→ Use `/resume <session_id>`. Session IDs are UUIDs visible via `/session` before you reset. Once set, Claude resumes that exact context on your next message.
```
/session             → shows: abc-123-...
/reset               → oops, session cleared
/resume abc-123-...  → restored, next message continues from where you left off
```

**Q: How do I switch Claude models mid-session?**
→ Use `/model claude-opus-4-6` (or any Claude model ID). The override is stored per session and takes effect on the next message. Use `/model` with no arguments to see the current model. `/reset` clears the override along with the session history.

**Q: How do I see token usage and cost?**
→ Send `/cost`. Usage is accumulated from every Claude turn in the session and persists across bot restarts. The cost figure comes directly from Claude Code's own accounting.

**Q: Can I check what Claude is doing mid-task?**
→ Yes — send `/status` at any time. The bot replies with the current tool call list and any text Claude has produced so far, identical to what is shown in the live placeholder message.

**Q: Can I use Feishu/QQ/DingTalk/WeCom instead of Telegram?**
→ Yes. Configure the relevant section in config.yaml. All platforms (except WeCom) use outbound WebSocket connections — no public IP needed. See the setup sections above for each platform.

**Q: Can I run multiple platforms at the same time?**
→ Yes. Set credentials for any combination of platforms in the same config file. lazycoding starts all configured adapters simultaneously and fans their events into one pipeline. Sessions and queuing are fully independent per conversation.

**Q: Do QQ/DingTalk/WeCom support streaming output like Telegram?**
→ Not edit-in-place streaming. These platforms don't support message editing. lazycoding sends a "Thinking…" message immediately, accumulates Claude's output, then sends the final response when done.

**Q: "Session contains expired thinking-block signatures" error**
→ This happens when Claude's extended thinking session has expired signature data. Send `/reset` to start a fresh session.
