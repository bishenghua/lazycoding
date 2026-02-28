# lazycoding 🛋️

> 躺着写代码 —— 通过 Telegram 聊天操控本地 Claude Code，同时管理多个工程。

lazycoding 是一个运行在本机的网关进程：把 Telegram 消息（文字 / 语音 / 文件）转发给本地 `claude` CLI，把流式输出实时回显到聊天窗口。每个 Telegram 对话（私聊 / 群组 / 频道）可以独立绑定一个工程目录，一个 bot 进程同时服务多个项目。

```
手机 Telegram
     │ 文字 / 语音 / 文件
     ▼
lazycoding（本机运行）
     │ 启动子进程
     ▼
claude CLI（--dangerously-skip-permissions）
     │ 读写文件、执行命令
     ▼
流式结果实时回显到 Telegram
```

---

## 目录

- [前置要求](#前置要求)
- [编译](#编译)
- [第一步：创建 Telegram Bot](#第一步创建-telegram-bot)
- [第二步：基础配置](#第二步基础配置)
- [第三步：获取 chat\_id](#第三步获取-chat_id)
- [第四步：配置多工程映射](#第四步配置多工程映射)
- [第五步：运行](#第五步运行)
- [命令](#命令)
- [交互模式](#交互模式)
- [语音输入](#语音输入)
- [文件上传](#文件上传)
- [文件下载](#文件下载)
- [进阶配置](#进阶配置)
- [常见问题](#常见问题)

---

## 前置要求

| 依赖 | 说明 |
|------|------|
| Go 1.21+ | 编译用，运行时不需要 Go 环境 |
| `claude` CLI | 已登录，`claude --version` 可正常输出 |
| Telegram Bot Token | 从 @BotFather 申请 |

验证 claude CLI 可用：

```bash
claude --version
claude --print "hello" --output-format stream-json --dangerously-skip-permissions
```

---

## 编译

```bash
git clone <repo>
cd lazycoding

# 标准构建（推荐）
go build -o lazycoding ./cmd/lazycoding/

# 启用内嵌 whisper.cpp 语音识别（需先 brew install whisper-cpp）
go build -tags whisper -o lazycoding ./cmd/lazycoding/
```

---

## 第一步：创建 Telegram Bot

1. 打开 Telegram，搜索 **@BotFather**
2. 发送 `/newbot`，按提示命名
3. BotFather 返回一串 Token，格式：`1234567890:ABCdefGHIjklMNOpqrsTUVwxyz`
4. 把 Token 填入 `config.yaml` 的 `telegram.token`

---

## 第二步：基础配置

复制配置文件并编辑：

```bash
cp config.yaml config.local.yaml
```

最小可用配置：

```yaml
telegram:
  token: "1234567890:ABCdefGHIjklMNOpqrsTUVwxyz"
  allowed_user_ids:
    - 123456789          # 你自己的 user_id（见下方获取方式）

claude:
  work_dir: "/Users/yourname/projects/my-project"
  timeout_sec: 300

log:
  format: "text"
  level: "info"
```

**获取 user\_id：** 在 Telegram 搜索 **@userinfobot**，发任意消息，回复中的 `Id` 就是。

---

## 第三步：获取 chat\_id

每个对话有唯一的 `chat_id`，配置多工程时需要用到。

### 用 /workdir 命令（推荐）

1. 用最小配置启动 bot：
   ```bash
   ./lazycoding config.local.yaml
   ```
2. 在目标对话里发：
   ```
   /workdir
   ```
3. 终端日志会打印：
   ```
   level=INFO msg="request started" conversation=-1001234567890 ...
   ```
   `-1001234567890` 就是该对话的 `chat_id`。

### 备选：@RawDataBot

在目标群组 @ **@RawDataBot** 发任意消息，回复中的 `"chat": {"id": ...}` 即为 `chat_id`。

### chat\_id 规律

| 值 | 对话类型 |
|----|---------|
| 正整数，如 `123456789` | 你与 bot 的私聊 |
| 负整数，如 `-1001234567890` | 群组 / 超级群组 / 频道 |

> ⚠️ YAML 里负数 chat\_id **必须加引号**：`"-1001234567890":`

---

## 第四步：配置多工程映射

```yaml
channels:

  # 你和 bot 的私聊 → 个人项目
  "123456789":
    work_dir: "/Users/yourname/projects/personal"

  # 群组 A → 后端 API 项目
  "-1001234567890":
    work_dir: "/Users/yourname/projects/backend-api"

  # 群组 B → 重要项目，指定更强的模型
  "-1009876543210":
    work_dir: "/Users/yourname/projects/frontend"
    extra_flags:
      - "--model"
      - "claude-opus-4-6"
```

未在 `channels` 中列出的对话，使用 `claude.work_dir` 作为工作目录。

**work\_dir 解析顺序（优先级从高到低）：**

```
channels.<chat_id>.work_dir   →   claude.work_dir   →   bot 启动目录
```

---

## 第五步：运行

```bash
./lazycoding config.local.yaml
```

推荐后台运行：

```bash
# tmux / screen
tmux new -s lazycoding
./lazycoding config.local.yaml

# nohup
nohup ./lazycoding config.local.yaml >> lazycoding.log 2>&1 &
```

**systemd 服务（Linux）：**

```ini
# /etc/systemd/system/lazycoding.service
[Unit]
Description=lazycoding Telegram Bot
After=network.target

[Service]
Type=simple
WorkingDirectory=/opt/lazycoding
ExecStart=/opt/lazycoding/lazycoding /opt/lazycoding/config.local.yaml
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

## 命令

| 命令 | 说明 |
|------|------|
| `/start` | 欢迎语 + 当前工作目录 |
| `/workdir` | 显示当前对话绑定的工作目录（兼查 chat\_id） |
| `/session` | 显示当前 Claude 会话 ID（调试用） |
| `/cancel` | 停止当前任务，**保留会话历史**（可继续对话） |
| `/reset` | 停止当前任务 + **清除会话历史**，重新开始 |
| `/download <路径>` | 从工作目录下载文件到 Telegram |
| `/help` | 显示命令列表 |

---

## 交互模式

### 内联取消按钮

每次 Claude 开始处理时，消息下方会出现 **[✕ 取消]** 按钮。点击即可立即停止当前任务（保留会话历史，队列也会清空）。

```
Bot：⏳ thinking…          [✕ 取消]
     ↓ Claude 思考中，随时可点取消
Bot：已完成分析，以下是...   （按钮自动消失）
```

### 快捷回复按钮

当 Claude 的回复末尾是一个**问句**时，bot 会自动在下方显示 **[Yes]** / **[No]** 快捷按钮，点击即直接发送回复：

```
Bot：需要同时更新单元测试吗？
     [Yes]  [No]

你：[点击 Yes]
Bot：好的，正在更新测试...
```

### 消息排队

Claude 处理期间发出的新消息会自动排队，处理完当前任务后立即执行下一条：

```
你：帮我分析这个文件
Bot：⏳ thinking…

你：（同时发）顺便也检查一下依赖
    （消息入队，等待中）

Bot：（第一条处理完毕）文件分析如下...
Bot：⏳ thinking…（开始处理第二条）
Bot：依赖检查如下...
```

---

## 语音输入

发送 Telegram 语音消息，bot 自动转文字后发给 Claude。支持四种后端：

| 方案 | 后端值 | 前置条件 | 隐私 |
|------|--------|---------|------|
| **A：Groq API**（推荐） | `groq` | 免费 API Key | 音频上传云端 |
| B：whisper-native（CGo） | `whisper-native` | `brew install whisper-cpp` + `-tags whisper` 构建 | 本地 |
| C：whisper.cpp CLI | `whisper-cpp` | `brew install whisper-cpp` | 本地 |
| D：openai-whisper | `whisper` | `pip install openai-whisper` | 本地 |

### 方案 A：Groq API（推荐，零安装）

```bash
# 1. 注册：https://console.groq.com → API Keys → Create key
# 2. 填入 config.yaml：
```

```yaml
transcription:
  enabled: true
  backend: "groq"
  groq:
    api_key: "gsk_your_key_here"
    model: "whisper-large-v3-turbo"   # 也可用 "whisper-large-v3"
    language: "zh"                     # 留空自动检测
```

免费额度：**每天 28,800 秒**（约 8 小时语音）。直接接受 OGG 格式，无需转码。

---

### 方案 B：whisper-native（CGo 内嵌，无独立进程）

```bash
brew install whisper-cpp   # 安装系统库 libwhisper
brew install ffmpeg         # OGG → WAV 转换（必须）

# 使用 CGo 标签构建
go build -tags whisper -o lazycoding ./cmd/lazycoding/
```

```yaml
transcription:
  enabled: true
  backend: "whisper-native"
  whisper_native:
    model: "base"       # 模型名（首次运行自动下载）或 .ggml 文件完整路径
    language: "zh"
```

首次运行时自动从 HuggingFace 下载模型到 `~/.cache/lazycoding/whisper/`（base 约 140 MB）。

---

### 方案 C：whisper.cpp CLI（本地，完全私有）

```bash
brew install whisper-cpp
whisper-download-ggml-model base
# 模型保存到 /opt/homebrew/share/whisper-cpp/models/ggml-base.bin
```

```yaml
transcription:
  enabled: true
  backend: "whisper-cpp"
  whisper_cpp:
    bin: "whisper-cli"
    model: "/opt/homebrew/share/whisper-cpp/models/ggml-base.bin"
    language: "zh"
```

若已安装 `ffmpeg`（`brew install ffmpeg`），bot 自动将 OGG 转为 WAV 再处理。

---

### 方案 D：openai-whisper（Python）

```bash
pip install openai-whisper   # 需要 Python 3 和 ffmpeg
```

```yaml
transcription:
  enabled: true
  backend: "whisper"
  whisper_py:
    bin: "whisper"
    model: "base"
    language: "zh"
```

---

### 语音模型参考

| 模型 | 大小 | 速度 | 适用场景 |
|------|------|------|---------|
| `tiny` | 75 MB | 极快 | 英文短句、追求速度 |
| `base` | 140 MB | 快 | **推荐起点** |
| `small` | 460 MB | 中等 | 含专业术语 |
| `medium` | 1.5 GB | 慢 | 高精度要求 |
| `large-v3-turbo` | 809 MB | 中等 | 高精度且较快 |

---

### 语音使用示例

```
你：[发送语音："帮我给 main.go 加上错误处理"]

Bot：🎤 识别文字：帮我给 main.go 加上错误处理
     _(thinking…)_
     _Running: Read: main.go_
     _Running: Edit: main.go_
     已完成，在第 23 行新增了 error 处理...
```

---

## 文件上传

直接在 Telegram 里把文件或图片发送到对话，bot 会自动：

1. 下载到该对话的**工作目录**
2. 告知 Claude 文件已就位，Claude 可直接操作

支持：**任意格式文件**（代码、PDF、文档等）、**图片**（截图、设计稿）

```
你：[上传 requirements.txt]
    caption: 根据这个依赖文件帮我写 Dockerfile

Bot：_Running: Read: requirements.txt_
     已分析依赖，Dockerfile 如下：
     ...
```

- Caption（说明文字）作为 Claude 指令；也可不填，之后单独发消息说明。
- 图片自动命名为 `photo_YYYYMMDD_HHMMSS.jpg`。
- 文件名中的目录信息会被自动去除（防路径穿越）。

---

## 文件下载

将工作目录中的文件发回 Telegram：

```
/download src/main.go
/download README.md
/download dist/app.tar.gz
```

路径相对于当前对话的工作目录，支持子目录，不能跨出工作目录。

```
你：帮我写一个数据处理脚本，保存为 process.py

Bot：_Running: Write: process.py_
     已创建 process.py...

你：/download process.py

Bot：[发送 process.py 文件]
```

---

## 进阶配置

### 多人共用一个 bot

```yaml
telegram:
  allowed_user_ids:
    - 111111111   # 你
    - 222222222   # 同事 A
    - 333333333   # 同事 B
```

`allowed_user_ids` 为空时允许所有人使用。同一对话同一时间只有一个 Claude 进程，后发的消息会**排队等待**，而非 kill 之前的任务。

### 指定 Claude 模型

```yaml
# 全局默认
claude:
  extra_flags:
    - "--model"
    - "claude-sonnet-4-6"

# 某对话单独覆盖
channels:
  "-1001234567890":
    work_dir: "/projects/important"
    extra_flags:
      - "--model"
      - "claude-opus-4-6"
```

### 调整超时

```yaml
claude:
  timeout_sec: 600   # 默认 300 秒，复杂任务可适当调大
```

### 日志 JSON 格式（接入日志系统）

```yaml
log:
  format: "json"
  level: "info"   # debug | info | warn | error
```

### 消息队列与中断

**队列行为：** 当 Claude 正在处理时，新消息会自动排队，待当前任务完成后按顺序执行。这样不会打断正在进行的任务，同时也不会丢失消息。

**主动取消：**
- 发送 `/cancel` 命令
- 或点击 Claude 回复上方的 **[✕ 取消]** 内联按钮

取消后队列中未处理的消息也会被清空。

### 终端对话日志

开启后，可以在服务端终端实时查看完整的对话过程（用户消息、工具调用、Claude 回复）：

```yaml
log:
  verbose: true   # 默认 false
```

开启效果：

```
15:04:05 ▶ conv=123456789  user:7846572322
  帮我给 main.go 加上错误处理

15:04:05   🔧 Read  {"path":"main.go"}
15:04:06   🔧 Edit  {"path":"main.go","old_string":"func process...
15:04:08 ◀ CLAUDE
  已完成。在第 23 行新增了 error 处理：
  - 检查 `os.Open` 返回的错误
  - 增加 `defer f.Close()`
```

`verbose: false`（默认）时终端只输出结构化 slog 日志，不打印对话内容。

---

## 常见问题

**Q：发消息后没有回复**
→ 检查 `allowed_user_ids` 是否包含你的 user\_id，或设为空（允许所有人）
→ 检查终端是否有错误日志
→ 确认 `claude` 在 bot 运行用户的 PATH 里：`which claude`

**Q：回复 "Error starting Claude"**
→ 手动验证 claude CLI：
```bash
cd /your/work_dir
claude --print "hello" --output-format stream-json --dangerously-skip-permissions
```

**Q：负数 chat\_id 在 YAML 里报解析错误**
→ 必须加引号：`"-1001234567890":` 而不是 `-1001234567890:`

**Q：重启后 /session 变了**
→ 正常，会话 ID 存在内存里，重启后清空。Claude 会自动开启新会话，不影响功能。

**Q：想让某个对话在 bot 启动目录运行**
→ 不配置该对话，并将 `claude.work_dir` 留空即可。

**Q：语音消息提示"语音识别未启用"**
→ 设置 `transcription.enabled: true` 并配置 backend，推荐 Groq（零安装）：
```yaml
transcription:
  enabled: true
  backend: "groq"
  groq:
    api_key: "gsk_..."
```

**Q：使用 whisper-cpp 时报 "command not found: whisper-cli"**
→ `brew install whisper-cpp`
→ 确认：`which whisper-cli`
→ 若命令名不同，在 config.yaml 指定完整路径：`bin: "/opt/homebrew/bin/whisper-cli"`

**Q：whisper-cpp 报 OGG 格式不支持**
→ 安装 ffmpeg：`brew install ffmpeg`（bot 自动使用）
→ 或改用 Groq backend（原生支持 OGG）

**Q：上传的文件去哪了？**
→ 保存在该对话的 `work_dir` 下，文件名与原始文件名相同。
→ `/workdir` 查看当前工作目录。

**Q：/download 提示"文件不存在"**
→ 路径是相对于工作目录的相对路径，例如：
```
工作目录: /projects/myapp
文件路径: /projects/myapp/src/main.go
命令:     /download src/main.go
```

**Q：whisper-native 编译报错**
→ 确认已安装系统库：`brew install whisper-cpp`
→ 使用正确构建命令：`go build -tags whisper ./cmd/lazycoding/`
