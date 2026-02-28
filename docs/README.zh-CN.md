# lazycoding 🛋️

[English](../README.md) · [简体中文](README.zh-CN.md)

**用手机通过 Telegram 操控本地 Claude Code。**

发一条消息，就能写代码、修 bug、管理多个项目。lazycoding 运行在你的机器上，把 Telegram 与本地 `claude` CLI 打通，每一次工具调用、每一行输出都实时回显到聊天窗口。

```
你（随时随地，任意设备）
        │  "重构支付模块并补充测试"
        ▼
   Telegram
        │
        ▼
   lazycoding  ← 运行在你的开发机上
        │  claude --print --output-format stream-json
        ▼
   Claude Code  ← 读写文件、执行命令、生成代码
        │
        ▼
   流式输出 → 实时回显到 Telegram 聊天窗口
```

---

## 为什么需要 lazycoding？

**Claude Code 很强大，但它被锁在终端里。** 离开电脑就失去了控制权。

| 没有 lazycoding | 有了 lazycoding |
|----------------|----------------|
| 必须坐在电脑前 | 手机、平板，任何 Telegram 客户端都可以 |
| 一次只能管一个项目 | 一个 bot 进程同时服务多个项目 |
| 重启后上下文丢失 | Session 持久化，重启后无缝续接 |
| 只能打字输入 | 支持语音，解放双手 |
| 文件只在本地 | 可直接在聊天中收发文件 |

**典型使用场景：**
- 在手机上 review PR，让 Claude 直接应用修改意见
- 通勤路上口述需求，Claude 在后台默默实现
- 在团队群里共享一个 Claude 编码助手
- 发起耗时任务后离开电脑，完成后收到回复通知

---

## 目录

- [前置要求](#前置要求)
- [快速上手](#快速上手)
- [编译](#编译)
- [第一步：创建 Telegram Bot](#第一步创建-telegram-bot)
- [第二步：基础配置](#第二步基础配置)
- [第三步：获取 chat\_id](#第三步获取-chat_id)
- [第四步：配置多工程映射](#第四步配置多工程映射)
- [第五步：运行](#第五步运行)
- [命令](#命令)
- [交互功能](#交互功能)
- [语音输入](#语音输入)
- [文件上传](#文件上传)
- [文件下载](#文件下载)
- [进阶配置](#进阶配置)
- [常见问题](#常见问题)

---

## 前置要求

| 依赖 | 说明 |
|------|------|
| Go 1.21+ | 仅编译需要，运行时不依赖 Go 环境 |
| `claude` CLI | 已登录，`claude --version` 可正常输出 |
| Telegram Bot Token | 从 @BotFather 申请，免费，2 分钟搞定 |

验证 claude CLI 可用：

```bash
claude --version
claude --print "hello" --output-format stream-json --dangerously-skip-permissions
```

---

## 快速上手

```bash
# 1. 克隆并编译
git clone https://github.com/bishenghua/lazycoding.git
cd lazycoding
make build

# 2. 创建配置文件
cp config.example.yaml config.yaml
# 编辑 config.yaml：填入 telegram.token、allowed_user_ids、claude.work_dir

# 3. 启动
./lazycoding config.yaml

# 4. 打开 Telegram，向 bot 发一条消息 —— Claude 开始工作
```

---

## 编译

```bash
# 标准构建（推荐）
go build -o lazycoding ./cmd/lazycoding/

# 含内嵌 whisper.cpp 语音识别（需 brew install whisper-cpp）
go build -tags whisper -o lazycoding ./cmd/lazycoding/

# 交叉编译所有平台 → dist/
make release
```

常用 Make 目标：

```
make build          为当前平台编译
make build-whisper  含 CGo whisper 语音识别
make test           运行测试
make release        交叉编译：linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64
make clean          清理产物
```

---

## 第一步：创建 Telegram Bot

1. 打开 Telegram，搜索 **@BotFather**，发送 `/newbot`
2. 按提示命名，BotFather 返回 Token，格式如 `1234567890:ABCdef…`
3. 将 Token 填入 `config.yaml` → `telegram.token`

---

## 第二步：基础配置

```bash
cp config.example.yaml config.yaml
# 编辑 config.yaml
```

最小可用配置：

```yaml
telegram:
  token: "1234567890:ABCdefGHIjklMNOpqrsTUVwxyz"
  allowed_user_ids:
    - 123456789   # 你自己的 Telegram user_id

claude:
  work_dir: "/Users/yourname/projects/my-project"
  timeout_sec: 900

log:
  format: "text"
  level: "info"
```

**获取 user\_id：** 在 Telegram 搜索 **@userinfobot**，发任意消息，回复中的 `Id` 就是。

---

## 第三步：获取 chat\_id

每个对话有唯一的 `chat_id`，配置多工程映射时需要用到。

### 用 /workdir 命令（推荐）

1. 启动 bot：`./lazycoding config.yaml`
2. 在目标对话里发 `/workdir`
3. 终端日志会打印：
   ```
   level=INFO msg="request started" conversation=-1001234567890 ...
   ```
   `-1001234567890` 就是该对话的 `chat_id`。

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

  # 私聊 → 个人项目
  "123456789":
    work_dir: "/Users/yourname/projects/personal"

  # 团队群 → 后端项目
  "-1001234567890":
    work_dir: "/Users/yourname/projects/backend-api"

  # 另一个群，使用更强的模型
  "-1009876543210":
    work_dir: "/Users/yourname/projects/ml-research"
    extra_flags:
      - "--model"
      - "claude-opus-4-6"
```

未在 `channels` 中列出的对话，使用 `claude.work_dir` 作为工作目录。

**work\_dir 解析优先级（从高到低）：**

```
channels.<chat_id>.work_dir  →  claude.work_dir  →  bot 启动目录
```

---

## 第五步：运行

```bash
./lazycoding config.yaml
```

推荐后台持久运行：

```bash
# tmux（开发推荐）
tmux new -s lazycoding
./lazycoding config.yaml

# nohup
nohup ./lazycoding config.yaml >> lazycoding.log 2>&1 &
```

**systemd 服务（Linux 生产环境）：**

```ini
# /etc/systemd/system/lazycoding.service
[Unit]
Description=lazycoding Telegram–Claude 网关
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

## 交互功能

### 内联取消按钮

每次 Claude 开始处理时，消息下方会出现 **[✕ Cancel]** 按钮，点击立即终止当前任务（会话历史保留，消息队列同时清空）。

```
Bot：⏳ thinking…   [✕ Cancel]
     ↓ 随时点击
Bot：⏹ Cancelled.
```

### 快捷回复按钮

当 Claude 的回复末尾是问句时，bot 自动显示 **[✅ Yes]** / **[❌ No]** 按钮，点击即直接发送回复。

```
Bot：需要同时更新单元测试吗？
     [✅ Yes]  [❌ No]

你：[点击 Yes]
Bot：⏳ thinking…
```

### 消息队列

Claude 处理期间发出的新消息会自动排队，按顺序处理，不会丢失，也不会打断正在进行的任务。消息入队时会立即收到确认通知。

```
你：分析这个文件
Bot：⏳ thinking…

你：（同时发）顺便检查一下依赖   ← 自动排队
Bot：⏳ 已排队，将在当前任务完成后处理。

Bot：分析结果：…
Bot：⏳ thinking…   ← 开始处理排队的消息
Bot：依赖检查：…
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

前往 [console.groq.com](https://console.groq.com) → API Keys → Create key，然后：

```yaml
transcription:
  enabled: true
  backend: "groq"
  groq:
    api_key: "gsk_your_key_here"
    model: "whisper-large-v3-turbo"
    language: "zh"   # 留空自动检测
```

免费额度：**每天 28,800 秒**（约 8 小时语音）。原生支持 OGG，无需转码。

### 方案 B：whisper-native（内嵌，无独立进程）

```bash
brew install whisper-cpp ffmpeg
go build -tags whisper -o lazycoding ./cmd/lazycoding/
```

```yaml
transcription:
  enabled: true
  backend: "whisper-native"
  whisper_native:
    model: "base"   # 首次使用自动下载到 ~/.cache/lazycoding/whisper/
    language: "zh"
```

### 方案 C：whisper.cpp CLI

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
    language: "zh"
```

### 语音模型参考

| 模型 | 大小 | 速度 | 适用场景 |
|------|------|------|---------|
| `tiny` | 75 MB | 极快 | 英文短句 |
| `base` | 140 MB | 快 | **推荐起点** |
| `small` | 460 MB | 中等 | 含专业术语 |
| `medium` | 1.5 GB | 慢 | 高精度要求 |
| `large-v3-turbo` | 809 MB | 中等 | 高精度且较快 |

---

## 文件上传

直接在 Telegram 里把文件或图片发到对话，bot 会自动：

1. 保存到该对话的**工作目录**
2. 告知 Claude 文件已就位，Claude 可直接操作

```
你：[上传 requirements.txt]
    caption：根据这个依赖文件帮我写 Dockerfile

Bot：🔧 Read: requirements.txt
     Dockerfile 如下：…
```

- Caption（说明文字）作为 Claude 指令；不填也可以，之后再单独发消息说明
- 图片自动命名为 `photo_YYYYMMDD_HHMMSS.jpg`
- 文件名中的目录信息会被自动去除（防路径穿越）

---

## 文件下载

将工作目录中的文件发回 Telegram：

```
/download src/main.go
/download dist/app.tar.gz
```

路径相对于当前对话的工作目录。

```
你：帮我写一个数据处理脚本，保存为 process.py
Bot：已创建 process.py

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

`allowed_user_ids` 为空时允许所有人使用。同一对话同一时间只有一个 Claude 进程，新消息自动排队。

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
  timeout_sec: 900   # 默认 900 秒（15 分钟），复杂任务可继续调大
```

### 终端对话日志

```yaml
log:
  verbose: true
```

开启后在终端实时打印完整对话过程：

```
15:04:05 ▶ conv=123456789  user:7846572322
  重构支付模块并补充测试

15:04:07   🔧 Read  {"file_path":"/projects/api/payment.go"}
15:04:09   🔧 Edit  {"file_path":"/projects/api/payment.go",...
15:04:15 ◀ CLAUDE
  已完成。将 PaymentService 提取为独立 struct，新增 interface，
  更新了 3 处调用点。
```

### Session 持久化

Claude 会话 ID 自动保存到 `~/.lazycoding/sessions.json`，重启 lazycoding 后无缝续接，无需任何额外配置。

会话以**工作目录路径**为 key（若已配置），而非对话 ID。这意味着同一个项目目录下的多个 Telegram 对话（例如手机私聊和桌面群组各一个）自动共享同一个 Claude 会话，请求之间也会自动串行化。

当指定工作目录没有已存储的 lazycoding 会话时，会自动扫描 `~/.claude/projects/` 发现本地 Claude Code CLI 留下的最近会话并恢复，让本地终端工作和 Telegram 无缝衔接。如果 lazycoding 已有该目录的存储会话，优先使用自己的（运行 `/reset` 后会触发自动发现）。

### JSON 日志格式（接入日志系统）

```yaml
log:
  format: "json"
  level: "info"
```

---

## 常见问题

**Q：发消息后没有回复**
→ 检查 `allowed_user_ids` 是否包含你的 user\_id（或设为空允许所有人）
→ 检查终端是否有错误日志
→ 确认 `claude` 在 PATH 里：`which claude`

**Q：回复 "Error starting Claude"**
```bash
cd /your/work_dir
claude --print "hello" --output-format stream-json --dangerously-skip-permissions
```

**Q：负数 chat\_id 在 YAML 里报解析错误**
→ 必须加引号：`"-1001234567890":` 而不是 `-1001234567890:`

**Q：任务超时（signal: killed）**
→ 增大 `claude.timeout_sec`。超时后 session 仍然保存，发"继续"即可接着做。

**Q：语音消息提示"Voice transcription is not enabled"**
→ 设置 `transcription.enabled: true` 并配置 backend，推荐 Groq（零安装）：
```yaml
transcription:
  enabled: true
  backend: "groq"
  groq:
    api_key: "gsk_..."
```

**Q：报 "command not found: whisper-cli"**
→ `brew install whisper-cpp`，再确认：`which whisper-cli`

**Q：whisper-cpp 报 OGG 格式不支持**
→ `brew install ffmpeg`（bot 自动使用）
→ 或改用 Groq backend（原生支持 OGG）

**Q：上传的文件去哪了？**
→ 保存在该对话的 `work_dir` 下，发 `/workdir` 查看路径。

**Q：/download 提示"File not found"**
→ 路径是相对于工作目录的相对路径：
```
工作目录: /projects/myapp
文件路径: /projects/myapp/src/main.go
命令:     /download src/main.go
```

**Q：重启后 session 是否会丢失？**
→ 不会。Session ID 持久化在 `~/.lazycoding/sessions.json`，重启后自动恢复。会话同样以工作目录为 key，同一项目目录的多个 Telegram 对话自动共享一个 Claude 会话。

**问：本地终端和 Telegram 能共享同一个 Claude 会话吗？**
→ 可以。当 lazycoding 没有该工作目录的存储会话时，会自动从 `~/.claude/projects/` 发现最近使用的会话并恢复。这意味着：
  - 在本地终端工作 → 切换到 Telegram 继续相同上下文
  - 在 Telegram 工作 → 然后用 `claude --resume <session-id>` 在本地继续（会话 ID 可通过 `/session` 查看）

如果 lazycoding 已有 stored session，优先使用自己的。运行 `/reset` 清除后，下次会自动发现最新的本地会话。

注意：不要同时在本地 CLI 和 Telegram 使用同一个会话，两个并发调用写入同一会话可能产生不可预期的结果。

**问：收到"Session contains expired thinking-block signatures"错误**
→ 这是 Claude 扩展思考模式的会话签名过期导致的。发送 `/reset` 开启新会话即可。
