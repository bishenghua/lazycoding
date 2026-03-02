# lazycoding – 架构与设计文档

## 设计动机

Claude Code 是一个强大的 AI 编程代理，但它被锁定在终端里——离开电脑，就失去了控制权。

lazycoding 解决的就是这个问题。它是一个**本地网关进程**，将 Claude Code 能力暴露给任意 Telegram 或飞书会话。设计遵循三个原则：

1. **本地运行** — Claude Code 在*你的*机器上执行，拥有*你的*文件系统的完整访问权限。源代码不经过任何云端中间层。
2. **多路复用** — 一个 bot 进程同时服务多个项目。每个 Telegram 对话映射到一个项目目录，对话之间完全隔离。
3. **可扩展** — 聊天平台、AI 后端、Session 存储、语音转文字，每个关键边界都抽象为接口，方便替换实现或接入新平台。

---

## 系统全景

```
┌──────────────────────────────────────────────────────────────┐
│                         开发者机器                            │
│                                                              │
│  ┌───────────────────────────────────────────────────────┐  │
│  │                      lazycoding                        │  │
│  │                                                       │  │
│  │  ┌─────────────┐   ┌────────────┐   ┌─────────────┐  │  │
│  │  │  channel/   │   │ lazycoding │   │   agent/    │  │  │
│  │  │ tg/feishu   │◄──│    核心    │──►│   claude    │  │  │
│  │  │  adapter    │   │  （调度层）│   │   runner    │  │  │
│  │  └─────────────┘   └────────────┘   └─────────────┘  │  │
│  │        │                │                  │           │  │
│  │   InboundEvent    session.Store        子进程          │  │
│  │   MessageHandle   FileStore           stream-json      │  │
│  └───────────────────────────────────────────────────────┘  │
│                                                              │
└──────────────────────────────────────────────────────────────┘
         ▲                                      ▼
  Telegram API（长轮询）               项目文件
  飞书 Webhook（HTTP）                /path/to/project/
```

---

## 目录结构

```
cmd/lazycoding/
  main.go                   入口：组装依赖，优雅退出

internal/
  config/
    config.go               Config 结构体、YAML 加载、默认值
                            WorkDirFor / ExtraFlagsFor 解析辅助

  agent/
    agent.go                Agent 接口、StreamRequest、Event 类型定义
    claude/
      runner.go             启动 claude 子进程，设置 WorkDir
      parser.go             stream-json JSONL → []agent.Event

  session/
    store.go                Store 接口、MemoryStore、FileStore（JSON 持久化）

  channel/
    channel.go              Channel 接口、InboundEvent、MessageHandle、
                            KeyboardButton（内联键盘）
    telegram/
      adapter.go            Telegram 轮询、语音/文件/图片处理、
                            内联键盘发送与回调应答、SendDocument
      renderer.go           Markdown→HTML 转换、表格渲染、
                            UTF-8 安全的 Split / Truncate
    feishu/
      adapter.go            飞书 Webhook 服务器、互动卡片发送/更新、
                            Token 管理、AES 事件解密、SendDocument
      renderer.go           Telegram HTML → Lark Markdown 转换、SplitText

  transcribe/
    transcribe.go           Transcriber 接口、Config、New() 工厂函数
    groq.go                 Groq 云端 Whisper API
    whisper_cpp.go          whisper.cpp CLI 子进程 + ffmpeg 转换
    whisper_py.go           openai-whisper Python CLI 子进程
    whisper_cgo.go          whisper.cpp CGo 绑定（构建标签：whisper）
    whisper_cgo_stub.go     标准构建的空实现

  lazycoding/
    lazycoding.go           编排层：dispatch、消息队列、consumeStream、
                            handleCommand、handleCallback、
                            handleDownload、handleLS、handleTree、handleCat
    convlog.go              终端人类可读对话日志（verbose 模式）

config.example.yaml         带注释的配置模板
```

---

## 核心接口

### `channel.Channel` — 平台抽象

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

`InboundEvent` 将所有入站流量（文字、语音、文件、命令、内联按钮点击）统一为一个结构体：

| 字段 | 含义 |
|------|------|
| `UserKey` | `"tg:{userID}"`（Telegram）或 `"fs:{openID}"`（飞书）— 发送者身份 |
| `ConversationID` | Telegram chat ID 字符串 — 决定使用哪个项目上下文 |
| `Text` | 消息文本；语音消息时为转录结果 |
| `IsCommand` | 消息以 `/` 开头时为 true |
| `Command` | 不含 `/` 的命令名，如 `"reset"` |
| `CommandArgs` | 命令名后的参数文本 |
| `IsVoice` | 文本来自语音转录时为 true |
| `IsCallback` | 内联键盘按钮点击时为 true |
| `CallbackID` | 必须通过 `AnswerCallback` 应答（消除 Telegram 加载动画） |
| `CallbackData` | 应用定义的载荷（如 `"cancel"`、`"yes"`） |

`MessageHandle.Seal()` 将消息标记为终态，阻止后续编辑（流式结束或任务取消后调用）。

---

### `agent.Agent` — AI 后端抽象

```go
type Agent interface {
    Stream(ctx context.Context, req StreamRequest) (<-chan Event, error)
}
```

`StreamRequest`：

| 字段 | 含义 |
|------|------|
| `Prompt` | 用户指令 |
| `SessionID` | 续接已有 Claude 会话；空 = 新会话 |
| `WorkDir` | Claude 的工作目录 |
| `ExtraFlags` | 附加 CLI 参数（如 `--model claude-opus-4-6`） |

`Event.Kind` 取值：

| 类型 | 载荷 | 触发时机 |
|------|------|---------|
| `EventKindInit` | `SessionID` | 第一个事件；提供本次运行的会话 ID |
| `EventKindText` | `Text` | Claude 增量文本输出 |
| `EventKindToolUse` | `ToolName`、`ToolInput`、`ToolUseID` | Claude 调用工具 |
| `EventKindToolResult` | `ToolUseID`、`ToolResult` | 工具返回结果 |
| `EventKindResult` | `SessionID`、`Text`、`Usage` | 最终事件；session ID 可能更新；Usage 携带 token 计数和费用 |
| `EventKindError` | `Err` | 不可恢复错误（超时、崩溃等） |

---

### `session.Store` — 持久化抽象

```go
type Store interface {
    Get(key string) (Session, bool)
    Set(key string, s Session)
    Delete(key string)
}
```

两种实现：
- **`MemoryStore`** — 进程内，重启后丢失（用于测试/嵌入场景）
- **`FileStore`** — JSON 文件持久化，保存至 `~/.lazycoding/sessions.json`，重启后自动恢复

生产入口（`cmd/lazycoding/main.go`）始终使用 `FileStore`。

---

### `transcribe.Transcriber` — 语音转文字抽象

```go
type Transcriber interface {
    Transcribe(ctx context.Context, audioPath string) (string, error)
}
```

四种后端通过配置切换，无需修改代码。

---

## 多工程映射

Session 存储和请求序列化均以 **`sessionKey`** 为键：若该对话配置了工作目录，则使用**工作目录路径**（`sessionKey = workDir`），否则退回到 **`ConversationID`**（Telegram chat ID 字符串）。原因：

- 同一项目目录的多个 Telegram 对话（例如手机私聊和桌面群组）共享同一个 Claude 会话，请求之间自动串行化。
- 群组中所有成员共享同一个 Claude 会话，可以看到彼此的进展，适合团队协作。
- 未配置工作目录时退回到 ConversationID，私聊之间天然隔离。

### 配置解析优先级（瀑布式）

```
channels["<chatID>"].work_dir    ← 最高优先级
channels["<chatID>"].extra_flags
        ↓
claude.work_dir                  ← 全局默认
claude.extra_flags
        ↓
（lazycoding 启动目录）           ← 最终兜底
```

---

## 请求完整生命周期

```
Telegram 更新到达
  └─ Adapter.toEvent()            [每条更新独立 goroutine，非阻塞]
       ├─ 命令？ → IsCommand = true
       ├─ 语音？ → downloadFile → Transcribe(ctx, oggPath) → IsVoice = true
       ├─ 文件？ → downloadFile → work_dir/filename
       ├─ 图片？ → downloadFile → work_dir/photo_*.jpg
       └─ 文字？ → Text = msg.Text
            │
            ▼ → 缓冲 channel（容量 16）
  lazycoding.Run()                [单一事件循环 goroutine]
       ├─ IsCallback → go handleCallback()   ← 内联按钮点击
       ├─ IsCommand  → go handleCommand()    ← 快速响应，不启动 Claude
       └─ 普通消息  → dispatch(ev)
                           │
                           ├─ 该对话已有 Claude 在运行？
                           │      是 → 追加到 queue
                           │          → SendText("⏳ 已排队…") 到该对话
                           │          → 返回
                           │      否 → startRequest(sessionKey（workDir 或 convID）, ev)
                           │
                           └─ startRequest()
                                  ├─ ctx, cancel = context.WithTimeout(900s)
                                  ├─ pending[sessionKey] = {cancel, done, queue}
                                  └─ go handleMessage(ctx, ev)
                                          ├─ WorkDirFor / ExtraFlagsFor
                                          ├─ store.Get(sessionKey) → sessionID
                                          │   （兜底：discoverLocalSession(workDir)）
                                          ├─ ag.Stream(ctx, req) → events chan
                                          ├─ go typingKeepalive（每 4 秒）
                                          ├─ SendKeyboard("⏳ thinking…",
                                          │    [[✕ Cancel]]) → handle
                                          └─ consumeStream(handle, events)
                                                  ├─ EventKindText    → 节流 UpdateText
                                                  ├─ EventKindToolUse → 更新占位消息
                                                  ├─ EventKindToolResult → 显示输出片段
                                                  ├─ EventKindResult  → 最终刷新 + Seal
                                                  │   （若内容 > 4096 字符：原消息保留工具摘要，
                                                  │    回复文本另发新消息）
                                                  └─ EventKindError   → Seal + 发送错误消息
                                                       （若为 thinking-signature 错误，提示执行 /reset）
                                                       │
                                                       ▼ goroutine 退出
                                               出队：若 queue 非空
                                                   → startRequest(sessionKey, queue[0])
```

---

## 流式更新策略

核心 UX 挑战：将终端流式输出映射到聊天消息。lazycoding 采用**原地编辑 + 节流**方案：

```
1. 发送占位消息："⏳ thinking…"  [✕ Cancel]
2. 随着事件到来：
   ├─ 工具调用 → 用工具名 + 截断输入更新占位消息
   │              工具返回时追加输出片段
   └─ 文本片段 → 累积到 strings.Builder
                  每隔 edit_throttle_ms（默认 1000ms）调用 UpdateText
3. 收到 EventKindResult：
   └─ 最终 UpdateText（完整 Markdown→HTML 渲染）
      Seal handle（不再编辑）
4. 若回复末尾是问句：
   └─ SendKeyboard 发送 [✅ Yes] [❌ No] 快捷回复按钮
```

| 事件 | 动作 |
|------|------|
| `EventKindInit` | 记录会话 ID |
| `EventKindText` | 追加到缓冲区；节流到期则 `UpdateText` |
| `EventKindToolUse` | 用工具名 + 输入替换占位消息 |
| `EventKindToolResult` | 在工具条目下追加截断后的输出 |
| `EventKindResult` | 最终刷新、`Seal`、可选快捷回复键盘；若内容 > 4096 字符：原消息保留工具摘要，回复文本另发新消息 |
| `EventKindError` | `Seal` + 发送错误消息；若为 thinking-signature 错误，提示用户执行 `/reset` |

### 工具输入格式化

`formatToolInput(toolName, input, workDir string) string`（定义于 `convlog.go`，同时用于终端 verbose 日志和 Telegram 消息构建）按工具类型从原始 JSON 中提取可读摘要：

| 工具 | 展示内容 |
|------|---------|
| `Read` / `Write` / `Edit` | 相对于 `workDir` 的路径；仍超 80 字符则显示末尾 3 段（加 `…/` 前缀） |
| `Bash` | 完整命令（最多 200 字符） |
| `Glob` | pattern + 缩短的目录路径 |
| `Grep` | pattern + 可选 glob 过滤 + 缩短的路径 |
| `WebFetch` | URL（最多 120 字符） |
| `WebSearch` | 查询字符串 |
| `Task` | 描述（最多 120 字符） |
| `AskUserQuestion` | 第一个问题文本（最多 120 字符） |
| `TodoWrite` | `(N todos)` |
| 其他 | 截断的原始 JSON（最多 160 字符） |

**消息长度限制：** Telegram 单条消息上限 4096 字节。收到 `EventKindResult` 时，若工具摘要加上回复文本超过 4096 字符，工具摘要更新到原占位消息，完整回复文本通过 `Split` 另发新消息（自动按 UTF-8 字符边界分割，不截断）。`UpdateText` 仍使用 `Truncate`（节流更新期间）。两者均在 UTF-8 字符边界处操作，不会切断多字节字符（如中文、Emoji）。

**HTML 渲染：** Claude 的输出经过 `MarkdownToTelegramHTML` 转换，支持代码块、Markdown 表格（Unicode 制表符绘制）、标题、加粗/斜体/删除线、行内代码、引用块、链接、无序列表。转义仅使用 Telegram 支持的四个命名实体（`&amp;` `&lt;` `&gt;` `&quot;`），不使用数字实体（如 `&#34;`）。

---

## Claude CLI 调用方式

```sh
claude \
  --print \
  --output-format stream-json \
  --dangerously-skip-permissions \
  [--resume <session_id>] \
  [extra_flags...] \
  "<prompt>"
```

- `stream-json` 每行输出一个 JSON 对象。
- `parser.ParseLineMulti` 将每行转换为零个或多个 `agent.Event`，处理单次 assistant 回复中包含多个 block（text + tool_use）的情况。
- `exec.CommandContext` 在 context 取消时（超时或 `/cancel`）保证 SIGKILL。
- stderr 被捕获，若非空则附加到错误消息中展示给用户。
- Scanner 缓冲区 4 MB，避免大型工具输出在解析层被截断。

---

## 语音输入流水线

```
Telegram 发来 OGG/OPUS 语音消息
  └─ handleVoice()
       ├─ downloadFile(fileID) → /tmp/lc-voice-<nano>.ogg
       └─ transcriber.Transcribe(ctx, oggPath) → text
            │
            ├─ backend="groq"
            │    └─ multipart POST → api.groq.com/v1/audio/transcriptions
            │       （原生支持 OGG，无需转码）
            │
            ├─ backend="whisper-native"
            │    └─ ffmpeg OGG→16kHz 单声道 WAV
            │       → whisper.cpp CGo 绑定 → []float32 采样 → 文本
            │       （首次使用自动从 HuggingFace 下载模型）
            │
            ├─ backend="whisper-cpp"
            │    └─ [ffmpeg OGG→WAV（若可用）]
            │       → exec whisper-cli 子进程 → 解析 .txt 输出
            │
            └─ backend="whisper"
                 └─ exec whisper Python 子进程 → 解析 .txt 输出
```

转录结果成为 `InboundEvent.Text`，`IsVoice=true`。编排层将其回显给用户（`🎤 Transcribed: …`）后转发给 Claude，让用户确认识别是否准确。

| 后端 | 安装 | OGG 支持 | 备注 |
|------|------|---------|------|
| `groq` | 仅需 API Key | 原生 | 推荐；每天免费 28800 秒 |
| `whisper-native` | `brew install whisper-cpp` | 经 ffmpeg | CGo；需 `-tags whisper` 构建 |
| `whisper-cpp` | `brew install whisper-cpp` | 经 ffmpeg | CLI 子进程 |
| `whisper` | `pip install openai-whisper` | 原生 | Python 子进程 |

---

## 文件上传流水线

```
Telegram 文件/图片更新
  └─ handleDocument() / handlePhoto()
       ├─ workDir = cfg.WorkDirFor(convID)
       ├─ sanitizeFilename() — 去除目录组件（防路径穿越）
       ├─ downloadFile(fileID) → workDir/<filename>
       └─ InboundEvent{Text: "[File saved to work directory: <name>]\n<caption>"}
```

事件文本即为发送给 Claude 的提示词——明确告知文件落地位置，Claude 无需额外指引即可操作。

---

## 文件系统命令

三个命令在 lazycoding 内部**直接执行**，不启动 Claude 子进程，响应即时。所有路径均经过 `safeJoin` 校验，必须在 `workDir` 范围内。

### `/ls [路径]`

```
/ls src/
  └─ safeJoin(workDir, "src/")
       └─ os.ReadDir(target)
            └─ 格式化每个条目：mode  size  mtime  name/
                 mode   = info.Mode().String()     如 "-rw-r--r--"
                 size   = formatFileSize(n)        如 "1.2K", "4.0M"
                 mtime  = ModTime().Format("Jan 02 15:04")
                 name   = 条目名称（目录加 "/"）
            └─ SendText("<pre>…</pre>")
```

### `/tree [路径]`

```
/tree
  └─ walk(workDir, prefix="", depth=0)
       ├─ 最大深度：   3
       ├─ 最大条目数： 150
       ├─ 跳过目录：   .git, node_modules, vendor, .cache, __pycache__, .next
       └─ SendText("<pre>…</pre>")
```

### `/cat <路径>`

```
/cat src/main.go
  └─ safeJoin(workDir, "src/main.go")
       └─ os.ReadFile(absPath)
            ├─ 截断：最多 200 行 或 8000 字节（先达到的限制生效）
            └─ SendText("<code>path</code>\n<pre>…</pre>[(truncated)])
```

## 文件下载流水线

```
/download src/main.go
  └─ safeJoin(workDir, "src/main.go")
       ├─ filepath.Clean(filepath.Join(workDir, rel))
       ├─ 验证结果以 workDir 为前缀（拒绝 ../../ 路径穿越）
       └─ ch.SendDocument(ctx, convID, absPath, rel)
```

---

## 并发模型

```
┌─ 1 个轮询 goroutine（Adapter.Events 循环）─────────────────────┐
│   每条更新独立 goroutine（下载 + 转录，非阻塞）               │
│   → 缓冲 channel（容量 16）                                   │
└───────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─ 1 个事件循环 goroutine（lazycoding.Run）──────────────────────┐
│   顺序读取 Events()                                           │
│   回调事件 → go handleCallback()   （快速）                   │
│   命令事件 → go handleCommand()    （快速）                   │
│   普通消息 → dispatch()                                       │
│     ├─ 忙碌？ → 追加到 queue（pendingState.queue，            │
│     │           由 pendingState.mu 保护）                     │
│     └─ 空闲？ → startRequest() → go handleMessage()          │
│                  ├─ context.WithTimeout                       │
│                  └─ 退出时：若 queue 非空则出队处理            │
└───────────────────────────────────────────────────────────────┘

pending  map[sessionKey → *pendingState]  (sessionKey = workDir 或 convID)  由 pendingMu（外锁）保护
pendingState.queue                        由 pendingState.mu（内锁）保护
```

**不变量：**
- 同一工作目录（或对话，若未配置工作目录）同一时刻至多运行**一个** Claude 子进程。
- 消息不会丢失，排队等待 Claude 就绪后依序处理。
- `cancelConversation()` 原子地取消子进程上下文并清空队列。
- 所有锁持有时间极短（临界区内无 I/O 操作）。

---

## 飞书 Adapter 设计

飞书 adapter 与 Telegram adapter 的主要差异：

| 维度 | Telegram | 飞书（默认） | 飞书（Webhook 模式） |
|------|----------|------------|-------------------|
| 事件推送方式 | 长轮询（`getUpdates`） | WebSocket 长连接 | HTTP Webhook（推送） |
| 需要公网 IP | ❌ 不需要 | ❌ 不需要 | ✅ 需要 |
| 服务器启动方式 | `Events()` 内部启动轮询 goroutine | `Events()` 主动拨号 `wss://` | `Events()` 启动 `http.Server` |
| 消息格式 | Telegram HTML（parse_mode=HTML） | Lark Markdown 包裹在互动卡片中 | Lark Markdown 包裹在互动卡片中 |
| 编辑消息 | `editMessageText` | PATCH `/im/v1/messages/{id}` | PATCH `/im/v1/messages/{id}` |
| Token 管理 | 静态 Bot Token | `tenant_access_token`（2h TTL，自动刷新） | `tenant_access_token`（2h TTL，自动刷新） |
| 事件去重 | 不需要（Telegram 保证） | `seen map[eventID]time.Time`，每 5 分钟清理 | `seen map[eventID]time.Time`，每 5 分钟清理 |
| AES 解密 | 无 | 无 | 可选；以 sha256(encrypt_key) 为 AES-256-CBC 密钥 |
| 语音/文件事件 | 完整支持 | 暂时忽略（待实现） | 暂时忽略（待实现） |

**WebSocket 模式（默认）：** adapter 调用 `POST /callback/ws/endpoint` 传入 app_id+app_secret，获取 `wss://` URL（含 `device_id`、`service_id` query param），再用 `gorilla/websocket` 主动拨号。帧格式为 protobuf 二进制（手写编解码器，不引入官方 SDK）。`method=0` = 控制帧（ping/pong），`method=1` = 数据帧（事件）。按 `ClientConfig.PingInterval`（默认 120s）发送 ping。每个事件帧立即回复 ACK（`payload={"code":200}`）。断线后指数退避重连（2s → 60s 上限）。

**Webhook 模式（`use_webhook: true`）：** adapter 启动 HTTP 服务器，飞书主动推事件。需要公网 IP 或内网穿透工具（ngrok/frp）。支持可选的 AES-CBC-256 事件解密。

**卡片格式：** 飞书消息以互动卡片形式发送，包含一个 `lark_md` div 元素（承载 Markdown 内容）和可选的 `action` 元素（承载按钮）。`UpdateText` 通过 PATCH 接口原地更新卡片内容，实现与 Telegram `editMessageText` 相同的流式效果。

**渲染器：** `TelegramHTMLToLarkMarkdown` 将 `MarkdownToTelegramHTML` 产生的 Telegram 风格 HTML 转换为 Lark Markdown（粗体 → `**...**`，代码块 → ` ``` `，链接 → `[text](url)`）。`SplitText` 在换行边界处分割长内容，遵守 `MaxCardTextLen = 3000` 字符限制。

---

## 交互功能设计

### 内联取消按钮

初始占位消息包含内联键盘 **[✕ Cancel]**。点击后：

1. Telegram 发送 `CallbackQuery` 更新。
2. `handleCallback()` 调用 `AnswerCallback`（消除 Telegram 加载动画）。
3. `cancelConversation(convID)` 取消 Claude 子进程的 context 并清空队列。
4. 发送 "⏹ Cancelled." 消息。

`tgHandle` 上的 `hasKeyboard` 标志确保首次 `UpdateText`（真实内容替换占位符）时自动移除按钮。

### 快捷回复按钮

`consumeStream` 返回后，`detectQuickReplies(finalText)` 检查最后一个非空行：若以 `?` 结尾，则发送独立的 `[✅ Yes]` / `[❌ No]` 键盘消息。点击后，按钮的 `Data` 字符串（`"yes"` 或 `"no"`）作为新的文本消息进入队列，按正常流程处理。

### Typing 保活

后台 goroutine 在 Claude 请求处理期间每 4 秒向对话发送一次 `SendTyping`。Telegram 的"正在输入…"指示器正常 5 秒后消失，保活机制确保它在长任务期间持续可见，让用户清楚机器人仍在工作。

### 排队通知

当消息在 Claude 运行时到达，会被入队，同时立即回复 `⏳ 已排队，将在当前任务完成后处理。`，用户不会因没有回应而产生疑惑。

### `/status` 查询

`Lazycoding` 结构体新增 `runningStatus sync.Map`（key = `sessionKey`，value = 当前渲染的 HTML 内容）：

1. `consumeStream` **启动时立即写入** `"(thinking…)"`，确保任何事件到来之前就有快照
2. **每次 `doFlush` 后更新**为最新内容（工具列表 + 已累积文字）
3. `consumeStream` 退出时通过 `defer` 删除

`/status` 命令读取此 map，将快照以新消息发出，用户可随时查看任务进度，不影响正在进行的占位消息。

---

## Session 持久化设计

```
Session{
    ClaudeSessionID   string    // 作为 --resume <id> 传给 claude CLI
    LastUsed          time.Time
    ModelOverride     string    // 本 session 的模型覆盖（可选）
    TotalCostUSD      float64   // 累计费用（跨多轮）
    TotalInputTokens  int       // 累计输入 token 数
    TotalOutputTokens int       // 累计输出 token 数
}
```

- **`/model <name>`** → `session.ModelOverride = name` → 每次请求以 `--model` flag 形式传入（替换 config `extra_flags` 中已有的 `--model`）
- **`/cost`** → 读取 session 中的 `TotalCostUSD`、`TotalInputTokens`、`TotalOutputTokens`
- session 保存改为**读-改-写**模式，确保 `ModelOverride`、usage 计数等字段跨轮次保留

`FileStore` 在每次 `Set` 或 `Delete` 时写穿到 `~/.lazycoding/sessions.json`（写穿缓存策略）。启动时 `NewFileStore` 读取文件；文件损坏或不存在时从空 map 开始，不会崩溃。

| 操作 | 结果 |
|------|------|
| 重启 lazycoding | sessions 从文件加载，Claude 上下文完整保留 |
| `/reset` | `store.Delete(sessionKey)`，Claude 开启新会话 |
| 手动删除 session 文件 | 所有对话重新开始，无害 |

### 会话 Key

会话以**工作目录路径**（若已配置）为 key，而非对话 ID。`sessionKey()` 返回 workDir（非空时），否则返回 convID。影响：

- 同一目录的多个 Telegram 对话共享一个 Claude 会话。
- 同一目录的请求串行化（同一目录同一时刻至多一个 Claude 子进程）。
- `/reset` 和 `/session` 命令也操作共享的 session key。

### 本地会话自动发现

当指定工作目录没有 stored session 时，`discoverLocalSession(workDir)` 扫描 `~/.claude/projects/<encoded>/` 下的 `.jsonl` 文件，返回最近修改的文件名（去掉 `.jsonl`）作为恢复 ID。

Claude Code 把路径中的每个 `/` 替换成 `-` 进行编码，因此 `/Users/hua/projects/foo` 对应 `~/.claude/projects/-Users-hua-projects-foo/`。

由于 Claude Code 会把所有会话（交互模式和 `--print` 模式）存在同一个项目目录下，lazycoding 可以透明地接续本地 CLI 开始的会话，反之亦然。

如果 lazycoding 已有 stored session，优先使用（运行 `/reset` 后才会触发自动发现）。

---

## 新增对话映射

1. 将 bot 添加到目标 Telegram 对话。
2. 发送 `/workdir`，终端日志显示 `conversation=<chatID>`。
3. 编辑 `config.yaml`：
   ```yaml
   channels:
     "<chatID>":
       work_dir: "/path/to/project"
   ```
4. 重启 lazycoding。**无需修改任何代码。**

---

## 扩展到其他平台

实现 `channel.Channel` 接口即可接入 Slack、Discord 或任何其他消息平台。核心编排层、Agent 运行器、Session 存储和语音转文字层全部与平台无关，只需在 `cmd/lazycoding/main.go` 中替换 adapter 即可。

**内置 adapter：**
- **Telegram** (`internal/channel/telegram`) — 长轮询、语音、文件上传/下载、内联键盘
- **飞书/Lark** (`internal/channel/feishu`) — HTTP Webhook、互动卡片、AES 事件解密、文件上传

**`cmd/lazycoding/main.go` 中的 adapter 初始化——两个平台可以同时启用：**
```go
var adapters []channel.Channel
if cfg.Feishu.AppID != ""   { adapters = append(adapters, fsadapter.New(cfg)) }
if cfg.Telegram.Token != "" { adapters = append(adapters, tgadapter.New(cfg, tr)) }
ch := channel.NewMultiAdapter(adapters...)  // 事件汇聚 + 路由
```

`channel.NewMultiAdapter` 将多个 adapter 包装为单一 `Channel` 接口。所有 adapter 的事件汇聚到同一个 channel；出站调用（`SendText`、`UpdateText` 等）通过 `conversationID → adapter` 路由表（随事件到达自动填充）转发回正确的 adapter。`MessageHandle` 被封装为 `multiHandle`，保留来源 adapter 引用以支持 `UpdateText` 路由。

**接入新平台（以 Slack 为例）：**
```go
slackCh, _ := slack.New(cfg)
b := lazycoding.New(slackCh, runner, store, cfg)
b.Run(ctx)
```

---

## 设计决策与权衡

### `--print` 批处理模式 vs PTY 交互模式

Claude Code 同时支持 PTY 交互模式和 `--print` 批处理模式。lazycoding 选择 `--print --output-format stream-json`，原因：

- **结构化输出** — `stream-json` 以机器可读的事件流输出（text、tool_use、tool_result、result），可按类型选择性渲染。
- **进程管理简洁** — `exec.CommandContext` + SIGKILL 取消机制可靠；PTY 生命周期管理复杂度更高。
- **与消息队列天然契合** — `--print` 每次处理一个请求，与每对话 FIFO 队列完美配合。

权衡：Claude 无法在任务中途发起交互式提问。快捷回复按钮在聊天层部分弥补了这一限制。

### 原地编辑 vs 发新消息

Telegram 支持在约 48 小时内编辑已发送的消息。原地编辑保持对话线程简洁（每次 Claude 响应对应一条消息），在手机上提供自然的"流式"体验。节流参数（`edit_throttle_ms`，默认 1000ms）防止触发 Telegram 的 429 速率限制。

对于超长回复（> 4096 字节），第一段作为主消息发送，后续段落作为跟进消息追加。

### Session key = 工作目录路径（兜底为对话 ID）

以工作目录路径为 key，意味着同一项目目录的所有 Telegram 对话共享同一个 Claude 会话——无论是私聊、群组，还是手机和桌面客户端各一个对话。这与团队共享代码库的工作模式一致。

没有配置 `work_dir` 时，退回到 `ConversationID` 作为 key，保持对话间的天然隔离。

若需要 per-user 隔离（例如每位开发者独立工作在自己的分支上），只需将 `sessionKey()` 函数改为使用 `UserKey`，改动极小。
