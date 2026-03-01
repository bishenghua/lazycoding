# 文件系统导航指令设计方案

> 日期：2026-03-01

## 背景

现有 IM 端（Telegram / QQ）通过配置文件静态绑定 `work_dir`，用户无法在对话中切换目录。
本方案新增四个 Unix 风格指令，让用户可以在运行时导航目录、查看文件、并在新目录开启 Claude 会话。

---

## 新增指令

| 指令 | 用法 | 说明 |
|------|------|------|
| `/pwd` | `/pwd` | 显示当前运行时目录（`/cd` 后的值） |
| `/cd` | `/cd <path>` | 切换当前目录 |
| `/ls` | `/ls [path]` | 列出目录内容 |
| `/new` | `/new` | 在当前目录开启全新 Claude 会话 |

与现有指令的关系：
- `/workdir`：保持不变，显示**配置文件**中的 `work_dir`
- `/pwd`：显示运行时的**实际当前目录**（初始值等于 `/workdir`，`/cd` 后会不同）
- `/reset`：保持不变，清 session 但不强调目录语义
- `/new`：语义上强调"在当前目录开新项目"，回复中明确说明使用的目录

---

## 核心机制：运行时 cwd

### 数据结构

在 `Lazycoding` struct 中增加内存 map（bot 重启后恢复配置值，不持久化）：

```go
cwd   map[string]string  // convID → 当前目录
cwdMu sync.RWMutex
```

### 辅助方法

```go
// currentDir 返回对话的实际当前目录。
// 若用户未执行过 /cd，回退到配置的 work_dir；
// 若配置也没有，返回空字符串（调用方按 "." 处理）。
func (lc *Lazycoding) currentDir(convID string) string {
    lc.cwdMu.RLock()
    defer lc.cwdMu.RUnlock()
    if d, ok := lc.cwd[convID]; ok {
        return d
    }
    return lc.cfg.WorkDirFor(convID)
}
```

### 对现有 dispatch 的影响

`dispatch`（处理普通消息）目前用 `lc.cfg.WorkDirFor(convID)` 决定 Claude 的工作目录。
改为调用 `lc.currentDir(convID)`，使 `/cd` 后发送的消息自动在新目录下执行。
这是本方案**唯一需要改动的非指令代码**。

---

## 指令详细设计

### `/pwd`

```
→ 当前目录：/home/user/projects/myapp
```

- 直接返回 `lc.currentDir(convID)`
- 若为空，返回 `"(lazycoding launch directory)"`

---

### `/cd <path>`

**路径解析规则（按优先级）：**

| 输入 | 解析结果 |
|------|----------|
| `~` 或 `~/foo` | `$HOME` 或 `$HOME/foo` |
| 绝对路径（`/foo/bar`） | 直接使用 |
| 相对路径（`src`、`..`） | 相对于 `currentDir(convID)` 解析 |

**验证：** `os.Stat` 确认路径存在且是目录，否则报错：

```
⚠️ 目录不存在：/home/user/nonexistent
```

**成功回复：**

```
📂 已切换到：/home/user/projects/myapp/src
```

**无参数时：** 切换到 `$HOME`（与 Unix `cd` 行为一致）。

---

### `/ls [path]`

列出 `currentDir(convID)`（或指定路径）下的内容。

**输出格式：**

```
📁 /home/user/projects/myapp
─────────────────────────────
📂 docs/
📂 src/
📄 README.md
📄 go.mod
📄 go.sum
📄 main.go
… 共 42 项，仅显示前 50
```

**规则：**
- 目录排在文件前面，各自按名称排序
- 目录名后附加 `/`
- 最多显示 50 条，超出时说明总数
- 隐藏文件（`.` 开头）默认不显示（与 `ls` 默认行为一致）
- 指定路径时，同样验证路径存在且是目录

---

### `/new`

在当前目录开启全新 Claude Code 会话（相当于在新 workspace 第一次使用 Claude）。

**行为：**
1. 取消当前正在运行的任务（若有）
2. 从 session store 中删除当前会话记录（清除 `ClaudeSessionID`）
3. 保留 `cwd[convID]`（保持当前目录不变）
4. 回复确认，明确说明新会话使用的目录

**成功回复：**

```
✨ 已在 /home/user/projects/myapp 开启新会话。
发送消息即可开始。
```

**与 `/reset` 的区别：**

| | `/reset` | `/new` |
|-|----------|--------|
| 清除 session | ✓ | ✓ |
| 取消当前任务 | ✓ | ✓ |
| 保留 cwd | ✓ | ✓ |
| 回复内容 | "Session reset. Starting fresh." | 明确显示新会话的工作目录 |
| 语义 | 重置对话 | 在当前目录开新项目 |

实现上 `/new` 复用 `/reset` 的逻辑，仅回复文案不同（附带当前目录）。

---

## 改动范围

```
internal/lazycoding/lazycoding.go   ← 主要改动
  + cwd map[string]string 字段
  + cwdMu sync.RWMutex 字段
  + currentDir(convID) 辅助方法
  + handleCommand: 新增 /pwd /cd /ls /new 四个 case
  ~ dispatch: WorkDirFor → currentDir（1 处替换）
```

无需改动：
- channel 接口（`channel.go`）
- QQ / Telegram adapter
- session store
- config
- 任何其他文件

---

## 安全考虑

- `/cd` 和 `/ls` 不限制路径范围（用户对自己的服务器有完全访问权），与现有 `/download` 的 `safeJoin` 策略不同——`/download` 限制在 work_dir 内是因为路径来自用户输入且会传文件，`/cd`/`/ls` 仅读目录元数据，风险较低
- 白名单（`allow_from`）已在 channel 层过滤，不在指令层重复校验
