# lazycoding 🛋️

> Code from anywhere — control a local Claude Code instance through Telegram, across multiple projects at once.

lazycoding is a gateway process that runs on your machine: it forwards Telegram messages (text / voice / files) to the local `claude` CLI and streams the output back to your chat in real time. Each Telegram conversation (DM / group / channel) can be bound to a separate project directory, so a single bot process serves multiple projects simultaneously.

```
Telegram (mobile)
     │  text / voice / file
     ▼
lazycoding  (runs on your machine)
     │  spawns subprocess
     ▼
claude CLI  (--dangerously-skip-permissions)
     │  reads/writes files, runs commands
     ▼
streaming output → Telegram
```

---

## Table of contents

- [Prerequisites](#prerequisites)
- [Build](#build)
- [Step 1 – Create a Telegram bot](#step-1--create-a-telegram-bot)
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
| Go 1.21+ | Build only; no Go runtime needed to run the binary |
| `claude` CLI | Must be logged in; `claude --version` should work |
| Telegram Bot Token | Obtain from @BotFather |

Verify the Claude CLI works:

```bash
claude --version
claude --print "hello" --output-format stream-json --dangerously-skip-permissions
```

---

## Build

```bash
git clone https://github.com/bishenghua/lazycoding.git
cd lazycoding

# Standard build (recommended)
go build -o lazycoding ./cmd/lazycoding/

# With embedded whisper.cpp voice recognition (requires brew install whisper-cpp)
go build -tags whisper -o lazycoding ./cmd/lazycoding/

# Or use Make
make build
make build-whisper
make release   # cross-compile for all platforms → dist/
```

---

## Step 1 – Create a Telegram bot

1. Open Telegram and search for **@BotFather**
2. Send `/newbot` and follow the prompts
3. BotFather replies with a token like `1234567890:ABCdefGHIjklMNOpqrsTUVwxyz`
4. Copy the token into `config.yaml` → `telegram.token`

---

## Step 2 – Basic configuration

```bash
cp config.example.yaml config.yaml
```

Minimal working config:

```yaml
telegram:
  token: "1234567890:ABCdefGHIjklMNOpqrsTUVwxyz"
  allowed_user_ids:
    - 123456789   # your user_id (see below)

claude:
  work_dir: "/Users/yourname/projects/my-project"
  timeout_sec: 900

log:
  format: "text"
  level: "info"
```

**How to find your user_id:** message **@userinfobot** on Telegram; the reply contains your `Id`.

---

## Step 3 – Find your chat\_id

Each conversation has a unique `chat_id`, needed when mapping multiple projects.

### Using the /workdir command (recommended)

1. Start the bot with the minimal config:
   ```bash
   ./lazycoding config.yaml
   ```
2. Send `/workdir` in the target conversation.
3. The terminal log prints:
   ```
   level=INFO msg="request started" conversation=-1001234567890 ...
   ```
   `-1001234567890` is the `chat_id` for that conversation.

### Alternative: @RawDataBot

Add **@RawDataBot** to the target group and send any message. The reply includes `"chat": {"id": ...}`.

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

  # Group A → backend API project
  "-1001234567890":
    work_dir: "/Users/yourname/projects/backend-api"

  # Group B → important project with a stronger model
  "-1009876543210":
    work_dir: "/Users/yourname/projects/frontend"
    extra_flags:
      - "--model"
      - "claude-opus-4-6"
```

Conversations not listed under `channels` use `claude.work_dir`.

**work\_dir resolution order (highest to lowest priority):**

```
channels.<chat_id>.work_dir  →  claude.work_dir  →  lazycoding launch directory
```

---

## Step 5 – Run

```bash
./lazycoding config.yaml
```

Recommended for long-running use:

```bash
# tmux
tmux new -s lazycoding
./lazycoding config.yaml

# nohup
nohup ./lazycoding config.yaml >> lazycoding.log 2>&1 &
```

**systemd service (Linux):**

```ini
# /etc/systemd/system/lazycoding.service
[Unit]
Description=lazycoding Telegram Bot
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

## Commands

| Command | Description |
|---------|-------------|
| `/start` | Welcome message + current work directory |
| `/workdir` | Show the work directory for this conversation (also reveals the chat\_id) |
| `/session` | Show the current Claude session ID (for debugging) |
| `/cancel` | Stop the current task, **keep session history** (conversation can continue) |
| `/reset` | Stop the current task + **clear session history**, start fresh |
| `/download <path>` | Download a file from the work directory to Telegram |
| `/help` | Show the command list |

---

## Interactive features

### Inline cancel button

When Claude starts processing a request, a **[✕ Cancel]** button appears on the placeholder message. Clicking it immediately stops the task (session history is kept; the queue is cleared too).

```
Bot: ⏳ thinking…          [✕ Cancel]
     ↓ click at any time
Bot: ⏹ Cancelled.
```

### Quick-reply buttons

When Claude's response ends with a question, the bot automatically shows **[✅ Yes]** / **[❌ No]** buttons. Clicking one sends that reply instantly.

```
Bot: Should I also update the unit tests?
     [✅ Yes]  [❌ No]

You: [click Yes]
Bot: ⏳ thinking…  (updates the tests)
```

### Message queuing

Messages sent while Claude is busy are queued and processed in order after the current task finishes — nothing is dropped or cancelled automatically.

```
You: Analyse this file
Bot: ⏳ thinking…

You: (while Claude is running) Also check the dependencies
     → queued

Bot: Analysis complete: …
Bot: ⏳ thinking…  (starts the queued message)
Bot: Dependency report: …
```

---

## Voice input

Send a Telegram voice message; the bot transcribes it and forwards the text to Claude. Four backends are supported:

| Option | Backend value | Prerequisite | Privacy |
|--------|---------------|--------------|---------|
| **A: Groq API** (recommended) | `groq` | Free API key | Audio uploaded to cloud |
| B: whisper-native (CGo) | `whisper-native` | `brew install whisper-cpp` + `-tags whisper` build | Local |
| C: whisper.cpp CLI | `whisper-cpp` | `brew install whisper-cpp` | Local |
| D: openai-whisper | `whisper` | `pip install openai-whisper` | Local |

### Option A: Groq API (recommended, zero install)

```yaml
transcription:
  enabled: true
  backend: "groq"
  groq:
    api_key: "gsk_your_key_here"   # console.groq.com → API Keys
    model: "whisper-large-v3-turbo"
    language: "en"                  # leave empty for auto-detect
```

Free tier: **28,800 seconds/day** (~8 hours of audio). Accepts OGG natively — no conversion needed.

---

### Option B: whisper-native (embedded CGo, no subprocess)

```bash
brew install whisper-cpp   # install libwhisper system library
brew install ffmpeg         # OGG → WAV conversion (required)

go build -tags whisper -o lazycoding ./cmd/lazycoding/
```

```yaml
transcription:
  enabled: true
  backend: "whisper-native"
  whisper_native:
    model: "base"    # model name (auto-downloaded) or absolute .ggml path
    language: "en"
```

On first run the model is downloaded automatically to `~/.cache/lazycoding/whisper/` (base ≈ 140 MB).

---

### Option C: whisper.cpp CLI (fully local)

```bash
brew install whisper-cpp
whisper-download-ggml-model base
# model saved to /opt/homebrew/share/whisper-cpp/models/ggml-base.bin
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

If `ffmpeg` is installed (`brew install ffmpeg`), OGG files are converted to WAV automatically.

---

### Option D: openai-whisper (Python)

```bash
pip install openai-whisper   # requires Python 3 and ffmpeg
```

```yaml
transcription:
  enabled: true
  backend: "whisper"
  whisper_py:
    bin: "whisper"
    model: "base"
    language: "en"
```

---

### Model reference

| Model | Size | Speed | Use case |
|-------|------|-------|----------|
| `tiny` | 75 MB | very fast | Short English phrases |
| `base` | 140 MB | fast | **Good starting point** |
| `small` | 460 MB | medium | Technical terminology |
| `medium` | 1.5 GB | slow | High accuracy required |
| `large-v3-turbo` | 809 MB | medium | High accuracy + reasonable speed |

---

### Voice example

```
You: [voice: "Add error handling to main.go"]

Bot: 🎤 Transcribed: Add error handling to main.go
     ⏳ thinking…
     🔧 Read: main.go
     🔧 Edit: main.go
     Done. Added error handling on line 23 …
```

---

## File upload

Send any file or photo directly to the Telegram conversation. The bot will:

1. Download it to the conversation's **work directory**
2. Tell Claude the file is ready so it can act on it immediately

Supported: **any file type** (source code, PDFs, documents) and **photos** (screenshots, designs).

```
You: [upload requirements.txt]
     caption: Write a Dockerfile based on these dependencies

Bot: 🔧 Read: requirements.txt
     Here is the Dockerfile: …
```

- The caption is used as the Claude instruction; you can also upload without a caption and describe what you want in a follow-up message.
- Photos are saved as `photo_YYYYMMDD_HHMMSS.jpg`.
- Directory components in file names are stripped (path traversal prevention).

---

## File download

Send a file from the work directory back to Telegram:

```
/download src/main.go
/download README.md
/download dist/app.tar.gz
```

Paths are relative to the conversation's work directory. Subdirectories are supported; you cannot escape the work directory.

```
You: Write a data-processing script and save it as process.py

Bot: 🔧 Write: process.py
     Created process.py …

You: /download process.py

Bot: [sends process.py]
```

---

## Advanced configuration

### Shared bot (multiple users)

```yaml
telegram:
  allowed_user_ids:
    - 111111111   # you
    - 222222222   # colleague A
    - 333333333   # colleague B
```

Leave `allowed_user_ids` empty to allow everyone. Only one Claude process runs per conversation at a time; new messages are queued until the current one finishes.

### Specify the Claude model

```yaml
# Global default
claude:
  extra_flags:
    - "--model"
    - "claude-sonnet-4-6"

# Override for a specific conversation
channels:
  "-1001234567890":
    work_dir: "/projects/important"
    extra_flags:
      - "--model"
      - "claude-opus-4-6"
```

### Adjust the timeout

```yaml
claude:
  timeout_sec: 900   # default 300 s; increase for complex long-running tasks
```

### JSON logging (for log aggregation)

```yaml
log:
  format: "json"
  level: "info"   # debug | info | warn | error
```

### Terminal conversation log

Enable a human-readable transcript of every conversation printed to stderr:

```yaml
log:
  verbose: true   # default false
```

Example output:

```
15:04:05 ▶ conv=123456789  user:7846572322
  Add error handling to main.go

15:04:05   🔧 Read  {"path":"main.go"}
15:04:06   🔧 Edit  {"path":"main.go","old_string":"func process...
15:04:08 ◀ CLAUDE
  Done. Added error handling on line 23:
  - Check the error returned by os.Open
  - Added defer f.Close()
```

With `verbose: false` (default) only structured slog entries are printed.

### Message queuing and cancellation

New messages arriving while Claude is running are **queued** and processed in order after the current task completes. To cancel the running task (and clear the queue):

- Send `/cancel`
- Or click the **[✕ Cancel]** inline button on the placeholder message

### Persistent sessions

Claude session IDs are stored in `~/.lazycoding/sessions.json` and survive process restarts. After a restart the bot resumes the same Claude session for each conversation, so context is not lost.

---

## FAQ

**Q: No response after sending a message**
→ Check that `allowed_user_ids` includes your user\_id (or leave it empty to allow everyone)
→ Check the terminal for error logs
→ Verify `claude` is in PATH for the user running the bot: `which claude`

**Q: "Error starting Claude" reply**
→ Manually verify the CLI:
```bash
cd /your/work_dir
claude --print "hello" --output-format stream-json --dangerously-skip-permissions
```

**Q: YAML parse error for a negative chat\_id**
→ It must be quoted: `"-1001234567890":` not `-1001234567890:`

**Q: /session changes after a restart**
→ Expected if session persistence is not configured. With `~/.lazycoding/sessions.json` (created automatically) sessions survive restarts.

**Q: Voice message says "Voice transcription is not enabled"**
→ Set `transcription.enabled: true` and configure a backend. Groq is the easiest (no install):
```yaml
transcription:
  enabled: true
  backend: "groq"
  groq:
    api_key: "gsk_..."
```

**Q: "command not found: whisper-cli" when using whisper-cpp**
→ `brew install whisper-cpp`
→ Confirm: `which whisper-cli`
→ If the binary has a different name, set the full path: `bin: "/opt/homebrew/bin/whisper-cli"`

**Q: whisper-cpp reports OGG format not supported**
→ Install ffmpeg: `brew install ffmpeg` (the bot uses it automatically)
→ Or switch to the Groq backend (accepts OGG natively)

**Q: Where did my uploaded file go?**
→ Saved in the conversation's `work_dir` under the original filename.
→ Run `/workdir` to see the current work directory.

**Q: /download says "File not found"**
→ The path is relative to the work directory:
```
Work dir:  /projects/myapp
File:      /projects/myapp/src/main.go
Command:   /download src/main.go
```

**Q: whisper-native build error**
→ Confirm the system library is installed: `brew install whisper-cpp`
→ Use the correct build tag: `go build -tags whisper ./cmd/lazycoding/`
