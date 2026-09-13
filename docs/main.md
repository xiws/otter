# otter design — AI Web 批量自动化 CLI

> 文档版本: v1  
> 最后更新: 2026-09-12  
> 状态: 设计阶段

---

## 1. 概述

**otter** 是一个命令行（CLI）工具，通过**浏览器自动化（Playwright/Puppeteer）** 或**逆向 Web API** 两种策略，将本地文件与 Prompt 批量发送给 ChatGPT、Gemini、DeepSeek 等 AI 大模型，并支持会话上下文管理与输出格式化。

### 1.1 核心目标

- **统一入口**: 一条命令完成"上传附件 + 发送 prompt + 获取回复"的全流程
- **多 Provider 支持**: ChatGPT、Gemini、DeepSeek 三个目标，相同的 CLI 体验
- **会话管理**: 隔离不同对话上下文，支持新建、选择、列举、删除会话
- **文件附件**: 支持传任意格式附件（代码、图片、PDF 等）到各 AI 平台
- **输出格式化**: 支持原始文本、Markdown 渲染等输出模式

### 1.2 非目标（Scope out）

- 不实现模型训练或微调
- 不实现代理 / 负载均衡 / 多账号轮换
- 不实现 GUI / TUI 界面
- 不实现插件系统

---

## 2. 架构总览

```
┌──────────────────────────────────────────────────┐
│                   CLI Layer                       │
│  otter gpt / gem / dep / new / select / list     │
├──────────────────────────────────────────────────┤
│               Dispatcher / Router                 │
├──────────┬──────────┬──────────┬─────────────────┤
│ Provider │ Provider │ Provider │  Session Mgr    │
│ ChatGPT  │  Gemini  │ DeepSeek │  (local store)  │
├──────────┴──────────┴──────────┴─────────────────┤
│                Control Strategy                    │
│  ┌─────────────────┐  ┌──────────────────────┐   │
│  │ Browser Driver  │  │ Direct API Adapter   │   │
│  │ (Playwright)    │  │ (Reverse Eng. API)   │   │
│  └─────────────────┘  └──────────────────────┘   │
├──────────────────────────────────────────────────┤
│              Config & Credential Store            │
│  ~/.otter/config.json      ~/.otter/sessions/    │
└──────────────────────────────────────────────────┘
```

### 2.1 分层职责

| 层 | 职责 |
|---|---|
| **CLI Layer** | 解析命令行参数、子命令、flag，分发到对应 handler |
| **Dispatcher** | 根据 Provider 类型路由到对应 Provider 实现 |
| **Provider** | 每个 AI 平台的逻辑封装（登录、发消息、收回复、解析） |
| **Session Manager** | 会话的增删改查、持久化、活动上下文维护 |
| **Control Strategy** | 浏览器自动化 vs 直接 API 调用，两个可切换的后端 |
| **Config Store** | JSON 配置文件的读写、环境变量覆盖 |

---

## 3. CLI 命令树

### 3.1 消息发送命令

```bash
# 基础格式
otter <provider> [flags] "<prompt>"

# 示例
otter gpt  -f ./docs/report.pdf "帮我润色这份报告"
otter gem  -f ./docs/code.py "解释这段代码" --type markdown
otter dep  "快速问答"                         # 不带附件
otter gpt  -f ./img1.png -f ./img2.jpg "对比两张图"
```

**通用 Flags**:

| Flag | 简写 | 类型 | 默认 | 说明 |
|---|---|---|---|---|
| `--file` / `-f` | | string[] | `[]` | 附件路径（可重复） |
| `--type` / `-t` | | string | `text` | 输出格式: `text`, `markdown`, `raw` |
| `--session` / `-s` | | string | `""` | 指定会话 ID（覆盖活动会话） |
| `--timeout` | | duration | `5m` | 请求超时 |
| `--no-stream` | | bool | `false` | 关闭流式输出 |
| `--browser` / `-b` | | bool | `false` | 强制使用浏览器模式（而非 API） |

**Provider 别名**:

| 别名 | 目标平台 | 默认策略 |
|---|---|---|
| `gpt` | ChatGPT (chatgpt.com) | Browser |
| `gem` | Gemini (gemini.google.com) | Browser |
| `dep` | DeepSeek (chat.deepseek.com) | Browser |

### 3.2 会话管理命令

```bash
# 创建新会话
otter new <provider> [--title "会话标题"]
# 输出: session-xxx (新会话 ID)

# 选择活动会话
otter select <provider> <session-id>
# 输出: "已切换到会话 <title> (session-xxx)"

# 列出会话
otter list <provider> [--all]
# 输出: 表格/列表，含 sessionId, title, 消息数, 最后活跃时间

# 删除会话
otter rm <provider> <session-id>
# 输出: "已删除会话 <session-id>"

# 查看会话详情
otter show <provider> <session-id> [--messages]
# 输出: 会话元数据，含 --messages 时显示消息摘要
```

### 3.3 配置命令

```bash
# 查看当前配置
otter config show

# 设置配置项
otter config set <key> <value>
# 示例: otter config set chatgpt.account user@example.com

# 检查各平台登录状态
otter auth check [provider]
# 输出: ✅/❌ 各平台 cookie/session 有效状态
```

### 3.4 全局 Flags

| Flag | 类型 | 默认 | 说明 |
|---|---|---|---|
| `--config` | string | `~/.otter/config.json` | 配置文件路径 |
| `--data-dir` | string | `~/.otter/` | 数据目录 |
| `--verbose` / `-v` | bool | `false` | 输出调试日志 |
| `--quiet` / `-q` | bool | `false` | 仅输出核心结果 |

---

## 4. Provider 抽象

### 4.1 通用接口

```go
// Provider 定义了所有 AI 平台必须实现的接口
type Provider interface {
    // 名称返回 provider 标识符
    Name() string

    // Send 发送消息并返回回复
    Send(ctx context.Context, req *SendRequest) (*SendResponse, error)

    // SendStream 流式发送
    SendStream(ctx context.Context, req *SendRequest) (<-chan StreamEvent, error)

    // Login 执行登录/认证（浏览器模式下由 Driver 完成）
    Login(ctx context.Context) error

    // ValidateSession 检查当前 cookie/session 是否有效
    ValidateSession(ctx context.Context) (bool, error)

    // GetSessionURL 返回该 provider 的 Web 会话页面 URL（用于调试）
    GetSessionURL(sessionID string) string
}

type SendRequest struct {
    Prompt    string
    Files     []FileAttachment
    SessionID string             // 若为空则创建新会话
    Timeout   time.Duration
}

type FileAttachment struct {
    Path     string
    MIMEType string // 由系统自动检测
    Size     int64
}

type SendResponse struct {
    SessionID string
    Content   string
    RawHTML   string        // 原始 HTML，便于不同格式输出
    TokenUsed int           // 预估 token（如果平台提供）
    Duration  time.Duration
}
```

### 4.2 各 Provider 策略

#### ChatGPT (chatgpt.com)

| 项目 | 值 |
|---|---|
| **Login** | Google OAuth（通过 browser driver） |
| **Auth persistence** | session token + cookies 序列化到 `~/.otter/sessions/chatgpt/cookies.json` |
| **Browser URLs** | `https://chatgpt.com/` |
| **API endpoints** (after login) | `POST https://chatgpt.com/backend-api/conversation` |
| **File upload** | 浏览器模拟拖拽/选择文件；API 模式通过 `POST /files/upload` |
| **Known limits** | ~4K context window (GPT-3.5) / ~32K (GPT-4), 文件大小 ~512MB |
| **Special notes** | 需要处理 Cloudflare 验证；会话对应 ChatGPT 的 conversation |

#### Gemini (gemini.google.com)

| 项目 | 值 |
|---|---|
| **Login** | Google OAuth（通过 browser driver） |
| **Auth persistence** | Google cookies + `__Secure-*` cookies 序列化 |
| **Browser URLs** | `https://gemini.google.com/app?hl=zh` |
| **API endpoints** | Gemini Web app 使用内部 gRPC-Web 协议 |
| **File upload** | Google Drive 中转或 browser 拖拽 |
| **Known limits** | 1M token context (Gemini 1.5 Pro), 文件通过 Google Cloud |
| **Special notes** | Gemini 没有正式的 conversation API；需要模拟 Web App 的 SSE 流 |

#### DeepSeek (chat.deepseek.com)

下表端点均经实测确认（2026-09）：

| 项目 | 值 |
|---|---|
| **Login** | `POST /api/v0/users/login`（json；邮箱填 `email`，手机号填 `mobile`，二者只用其一） |
| **Auth persistence** | `auth_token` cookie + localStorage |
| **Browser URLs** | `https://chat.deepseek.com/` |
| **创建会话** | `POST /api/v0/chat_session/create` → 返回会话 UUID |
| **发送消息** | `POST /api/v0/chat/completion` → `text/event-stream` |
| **File upload** | `POST /api/v0/file/upload_file`（multipart，字段名 `file`） |
| **文件状态查询** | `GET /api/v0/file/fetch_files?file_ids=<id>` |
| **PoW 挑战** | `POST /api/v0/chat/create_pow_challenge` |
| **Known limits** | ~64K context, 文件大小 ~100MB |
| **Special notes** | 除登录外所有写操作都要求 `x-ds-pow-response` 请求头；未知路径不返回 404，而是回落到单页应用的 HTML |

**PoW 防护（重要）**

DeepSeek 对对话、文件上传等接口加了工作量证明校验，缺少请求头时返回
`{"code":40300,"msg":"MISSING_HEADER"}`。流程是：

1. `POST /api/v0/chat/create_pow_challenge`，body 为 `{"target_path":"<接口路径>"}`；
2. 求解挑战得到 `answer`（见下）；
3. 把 `{algorithm, challenge, salt, answer, signature, target_path}` 做 base64 编码，
   放进 `x-ds-pow-response` 请求头。

求解算法标识为 `DeepSeekHashV1`，由 DeepSeek 前端分发的 WebAssembly 模块实现
（内部是内联的 SHA3-256 与 Keccak-f 置换，非标准库调用）。该模块未公开算法细节，
因此 `pkg/pow` 直接嵌入并调用官方模块（`pkg/pow/sha3_wasm.wasm`），
运行时采用纯 Go 的 `wazero`，无 CGO 依赖。`difficulty`（通常 144000）是搜索次数上界，
难度 144000 时单次求解约十余毫秒。

**会话与多轮**

- 平台侧会话 ID 是服务端生成的 UUID，本地 `sess_*` 需要先换取它，
  存在会话元数据的 `external_id` 字段；
- 首轮 `parent_message_id` 传 `null`；后续追问必须传上一轮的
  `response_message_id`（存在元数据 `parent_message_id`），否则上下文会丢失。

**响应流（SSE）格式**

`/api/v0/chat/completion` 返回 `text/event-stream`，正文增量有两种形态，
两种都要收集：

```
data: {"p":"response/content","o":"APPEND","v":"你"}
data: {"v":"好"}                         ← 没有 p 字段
data: {"p":"response/status","v":"FINISHED"}
```

注意两个坑：

- **短回复的内容只存在于初始帧**。初始帧 `{"v":{"response":{...,"content":"4"}}}`
  里的 `content` 是已累积内容快照；长回复该字段为空、靠增量帧累加，
  而服务端一次生成的短回复不再发增量帧，漏读快照会导致整条回复丢失。
- **HTTP 200 下也可能是业务错误**，如 `{"data":{"biz_code":9,"biz_msg":"invalid ref file id"}}`，
  必须当作错误上报，否则调用方只看到空回复。

另：文件上传是异步的，刚上传时 `status` 为 `PENDING`，此时作为 `ref_file_ids`
引用会被拒（`biz_code=9`），需轮询到 `SUCCESS` 再发送。

---

## 5. 控制策略：Browser vs Direct API

### 5.1 浏览器自动化模式（Browser Driver）

**适用场景**: ChatGPT、Gemini 的反向 API 不稳定/未公开时；需要处理验证码

**技术选型**: **Playwright for Go**（`github.com/playwright-community/playwright-go`）

**实现要点**:

```go
// BrowserDriver 封装 Playwright 浏览器控制
type BrowserDriver struct {
    pw     *playwright.Playwright
    browser playwright.Browser
    page   playwright.Page

    // 已登录的 cookie 缓存
    cookiesPath string
}

func (d *BrowserDriver) Launch(headless bool) error
func (d *BrowserDriver) Navigate(url string) error
func (d *BrowserDriver) Type(selector string, text string) error
func (d *BrowserDriver) Click(selector string) error
func (d *BrowserDriver) Upload(selector string, files []string) error
func (d *BrowserDriver) WaitForResponse(urlPattern string) (string, error)
func (d *BrowserDriver) ExtractText(selector string) (string, error)
func (d *BrowserDriver) SaveCookies() error
func (d *BrowserDriver) LoadCookies() error
func (d *BrowserDriver) Close()
```

**工作流 — 发送消息**:

1. Launch 浏览器（headless 默认，`--browser-visible` 开启可见）
2. LoadCookies 恢复已存 cookie
3. Navigate 到对应 URL
4. ValidateSession — 如果 cookie 过期，弹出 Login 流程（首次自动，后续可 `otter auth check` 手动触发）
5. 如果存在活动会话，Navigate 到会话 URL；否则新建
6. Upload 附件文件
7. Type prompt 到输入框
8. Click 发送按钮
9. WaitForResponse 等待回复完毕
10. ExtractText 获取回复内容
11. 返回结果，SaveCookies（如有更新）

### 5.2 直接 API 模式（Direct API）

**适用场景**: DeepSeek（API 规整）；未来扩展至 OpenAI API、Gemini API 等官方 API

**实现要点**:

```go
// DirectAPI 封装 HTTP 请求实现
type DirectAPI struct {
    client     *http.Client
    baseURL    string
    authToken  string
    sessionMgr *SessionManager
}

func (a *DirectAPI) Send(ctx context.Context, req *SendRequest) (*SendResponse, error)
func (a *DirectAPI) UploadFile(ctx context.Context, path string) (string, error)
func (a *DirectAPI) RefreshToken(ctx context.Context) error
```

**工作流 — DeepSeek Direct API**:

1. 从 config 读取 auth_token（或从 cookie 缓存恢复）
2. 若本地会话还没有平台 ID，`POST /api/v0/chat_session/create` 换取会话 UUID
3. 附件先 `POST /api/v0/file/upload_file`，再轮询 `GET /api/v0/file/fetch_files`
   直到 `status=SUCCESS`（上传是异步的，PENDING 时引用会被拒）
4. 每个写操作前申请 PoW 挑战并求解，结果放进 `x-ds-pow-response`
5. `POST /api/v0/chat/completion` 发送消息，按 SSE 解析回复
6. 记录本轮 `response_message_id`，下轮追问作为 `parent_message_id`
7. 返回结果

### 5.3 策略选择逻辑

```
FOR 每次 Send/SendStream 调用:
  IF 命令行指定 --browser:
     使用 BrowserDriver
  ELSE IF provider 有稳定的 Direct API 实现:
     使用 DirectAPI
  ELSE:
     使用 BrowserDriver
```

---

## 6. 会话管理

### 6.1 数据模型

```go
type Session struct {
    ID          string    `json:"id"`          // 生成: "sess_" + 随机 id
    Provider    string    `json:"provider"`     // "chatgpt" | "gemini" | "deepseek"
    Title       string    `json:"title"`        // 用户自定义标题或自动摘要
    CreatedAt   time.Time `json:"created_at"`
    UpdatedAt   time.Time `json:"updated_at"`
    MessageCount int      `json:"message_count"`

    // 平台内部 ID（用于恢复已有对话）
    ExternalID  string    `json:"external_id,omitempty"`

    // Provider 特有的上下文元数据
    Metadata    map[string]string `json:"metadata,omitempty"`
}

type Message struct {
    ID        string    `json:"id"`
    SessionID string    `json:"session_id"`
    Role      string    `json:"role"`       // "user" | "assistant"
    Content   string    `json:"content"`
    Files     []string  `json:"files,omitempty"` // 附件路径
    CreatedAt time.Time `json:"created_at"`
    TokenUsed int       `json:"token_used,omitempty"`
}
```

### 6.2 存储结构

```
~/.otter/
├── config.json               # 全局配置
├── sessions/
│   ├── chatgpt/
│   │   ├── cookies.json      # 浏览器 cookie 持久化
│   │   ├── index.json        # 该 provider 所有会话索引
│   │   └── sess_xxxxx.json   # 单个会话 + 消息记录
│   ├── gemini/
│   │   └── ...
│   └── deepseek/
│       └── ...
└── active_session.json       # 当前活动的 session ID（可选）
```

### 6.3 Session Manager 接口

```go
type SessionManager struct {
    dataDir string
}

// ActiveSession 返回当前 provider 的活动会话（~/.otter/active_session.json）
func (m *SessionManager) ActiveSession(provider string) (*Session, error)

// SetActive 设置活动会话
func (m *SessionManager) SetActive(sessionID string) error

// Create 创建新会话
func (m *SessionManager) Create(provider string, title string) (*Session, error)

// Get 获取会话
func (m *SessionManager) Get(sessionID string) (*Session, error)

// List 列出 provider 的所有会话
func (m *SessionManager) List(provider string) ([]*Session, error)

// Delete 删除会话及其消息
func (m *SessionManager) Delete(sessionID string) error

// AddMessage 添加消息到会话
func (m *SessionManager) AddMessage(sessionID string, msg *Message) error

// GetMessages 获取会话消息列表
func (m *SessionManager) GetMessages(sessionID string) ([]*Message, error)
```

### 6.4 会话生命周期

```
用户执行 otter new gpt
  → SessionManager.Create("chatgpt", "新会话")
  → 写入 sessions/chatgpt/sess_xxx.json
  → 设置 sessions/chatgpt/index.json 更新
  → provider 侧也创建新的 conversation（通过 browser 或 API）
  → 记录 provider 返回的 external_id
  → 输出 session ID

用户执行 otter select gpt sess_abc
  → 写入 active_session.json: { provider: "gpt", session_id: "sess_abc" }
  → 后续 otter gpt "..." 默认使用该会话

用户执行 otter gpt "你好"
  → 读取 active_session.json 获取 sess_abc
  → 从 sessions/chatgpt/sess_abc.json 恢复上下文（历史消息）
  → 发送到 provider
  → 接收结果，追加到消息列表
  → 更新会话文件
```

---

## 7. 配置文件

### 7.1 配置文件路径

`~/.otter/config.json`

### 7.2 配置结构

```json
{
  "chatgpt": {
    "account": "user@example.com",
    "password": "",
    "auth_type": "google",
    "cookies_path": "~/.otter/sessions/chatgpt/cookies.json"
  },
  "gemini": {
    "account": "user@example.com",
    "password": "",
    "auth_type": "google",
    "cookies_path": "~/.otter/sessions/gemini/cookies.json"
  },
  "deepseek": {
    "account": "15170000000",
    "password": "",
    "auth_type": "deepseek",
    "cookies_path": "~/.otter/sessions/deepseek/cookies.json"
  },
  "global": {
    "browser_visible": false,
    "default_timeout": "5m",
    "default_output": "text",
    "data_dir": "~/.otter/"
  }
}
```

### 7.3 凭据安全

- 密码字段**不建议**明文存储在配置文件中
- 优先利用浏览器 **cookie 持久化**（首次手动登录后，后续复用 cookie）
- 支持环境变量覆盖: `OTTER_CHATGPT_PASSWORD`, `OTTER_GEMINI_PASSWORD`, `OTTER_DEEPSEEK_PASSWORD`
- 未来可集成 macOS Keychain（`security` 命令行）或 1Password CLI
- `.gitignore` 需包含 `~/.otter/` 整个目录

---

## 8. 输出格式化

### 8.1 输出模式

| 模式 | CLI flag | 行为 |
|---|---|---|
| `text` | `--type text` (默认) | 纯文本输出，移除 HTML 标签 |
| `markdown` | `--type markdown` | 保留 Markdown 格式，在支持的终端中渲染 |
| `raw` | `--type raw` | 原始 HTML 输出（调试用） |
| `silent` | `--quiet` | 只写文件，不输出到 stdout（与 `-o file` 配合） |

### 8.2 输出目标

```bash
# 输出到 stdout（默认）
otter gpt "你好"

# 输出到文件
otter gpt "你好" -o ./output.md

# 输出到 stdout 并 append 到文件
otter gpt "你好" -o ./conversation.log --append

# 复制到剪贴板（macOS）
otter gpt "你好" | pbcopy
# 或内置 flag
otter gpt "你好" --clipboard
```

### 8.3 流式输出

```bash
otter gpt "写一首诗" --stream
# 一行一行实时输出，类似 ChatGPT 打字效果
# 终端使用 \r 覆盖或逐行追加
```

---

## 9. 错误处理与异常

### 9.1 常见错误场景

| 场景 | 行为 |
|---|---|
| Cookie 过期 | 输出提示 `"登录已过期，请执行 otter auth login <provider>"` |
| 网络超时 | 输出错误并建议 `--timeout` 或检查网络 |
| 文件过大 | 输出文件大小与当前 provider 限制 |
| 不支持的文件类型 | 输出支持的 MIME 类型列表 |
| Provider 服务不可用 | 输出 HTTP 状态码和响应片段 |
| 浏览器启动失败 | 输出二进制缺失提示（`playwright install`） |

### 9.2 退出码

| 退出码 | 含义 |
|---|---|
| `0` | 成功 |
| `1` | 通用错误 |
| `2` | 配置错误（文件未找到、格式错误） |
| `3` | 认证错误（cookie 过期、登录失败） |
| `4` | Provider 错误（API 拒绝、限流） |
| `5` | 输入错误（文件不存在、格式不支持） |
| `6` | 浏览器错误（Playwright 未安装、版本不匹配） |

---

## 10. 项目结构

```
otter/
├── cmd/
│   └── cli/
│       └── main.go              # 入口，初始化 cobra root command
├── pkg/
│   ├── cli/
│   │   ├── root.go              # cobra 根命令与全局 flags
│   │   ├── send.go              # send 类子命令（gpt/gem/dep）
│   │   ├── session.go           # new/select/list/rm/show
│   │   └── config.go            # config show/set
│   ├── provider/
│   │   ├── provider.go          # Provider 接口定义
│   │   ├── chatgpt.go           # ChatGPT 实现
│   │   ├── gemini.go            # Gemini 实现
│   │   └── deepseek.go          # DeepSeek 实现
│   ├── browser/
│   │   ├── driver.go            # BrowserDriver（Playwright 封装）
│   │   └── login.go             # 各平台登录流程
│   ├── api/
│   │   ├── client.go            # HTTP client 封装
│   │   └── deepseek_api.go      # DeepSeek Direct API 实现
│   ├── session/
│   │   ├── session.go           # Session 结构体与 SessionManager
│   │   └── message.go           # Message 结构体
│   ├── config/
│   │   └── config.go            # 配置读写与校验
│   └── output/
│       ├── formatter.go         # 输出格式选择与渲染
│       └── stream.go            # 流式输出
├── internal/
│   └── script/
│       └── browser_login.js     # 浏览器登录辅助脚本（可选）
├── docs/
│   └── main.md                  # 本文档
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

---

## 11. 实现优先级

### Phase 1 — CLI 骨架 + DeepSeek Direct API (MVP)

```
1. 项目初始化：cobra 命令行框架，root command，全局 flags
2. Config 模块：配置文件的读取与校验
3. Session Manager：会话的增删改查、活动会话、本地存储
4. DeepSeek Direct API：
   -   /api/v0/users/login 登录（手机号走 mobile、邮箱走 email）
   -   /api/v0/chat/create_pow_challenge + pkg/pow 求解 PoW
   -   /api/v0/chat_session/create 换取平台会话 UUID
   -   /api/v0/file/upload_file 上传附件 + /api/v0/file/fetch_files 等待就绪
   -   /api/v0/chat/completion 发送消息并按 SSE 接收
5. CLI 命令：otter dep, otter new, otter select, otter list
6. 输出格式化：text, markdown 模式
```

### Phase 2 — 浏览器自动化

```
1. Playwright 集成：BrowserDriver 封装
2. Cookie 持久化：登录状态复用
3. ChatGPT 浏览器模式：登录 → 上传 → 发送 → 收集回复
4. Gemini 浏览器模式：同上（适配不同页面结构）
5. CLI 命令：otter gpt, otter gem
6. otter auth check 命令
```

### Phase 3 — 增强与优化

```
1. 流式输出（SSE / WebSocket）
2. 输出到文件、剪贴板
3. 错误处理完善、重试机制
4. --verbose 调试日志
5. macOS Keychain 集成
6. 批量文件处理
7. 并发发送到多个 provider
```

---

## 12. 依赖管理

### Go 依赖（`go.mod`）

| 包 | 用途 | 优先级 |
|---|---|---|
| `github.com/spf13/cobra` | CLI 框架 | P0 |
| `github.com/playwright-community/playwright-go` | 浏览器自动化 | P1 |
| `github.com/xiws/orca` | 已有基础设施（间接依赖） | — |
| 标准库 `encoding/json`, `net/http`, `os/exec` | — | — |

### 系统依赖

- **Playwright 浏览器二进制**: 通过 `npx playwright install chromium` 安装
- macOS Chrome/Chromium（可选，用于可见模式调试）

---

## 13. 设计原则与约定

### 代码约定

1. **最小外部依赖**: 优先使用标准库，非必要不引入第三方包
2. **可测试**: Provider 接口和 SessionManager 通过 mock 可测试
3. **可观察**: 每个关键步骤有 `--verbose` 日志输出
4. **健壮的失败**: 所有外部调用有超时和重试（最多 2 次），重试间隔指数退避
5. **配置文件不提交**: `~/.otter/` 加入 `.gitignore`

### 安全约束

1. 密码优先从环境变量读取，其次配置文件
2. cookie 文件建议设置 `0600` 权限
3. 不记录任何凭证到日志
4. 不将配置提交到版本控制

### 关于 Browser 登录流程

浏览器模式下的**首次登录**需要用户交互（Google OAuth 弹窗、手机验证等）。策略如下：

1. 首次执行时启动 visible 浏览器（`headless: false`）
2. 打开登录页面（Google 登录 / DeepSeek 登录）
3. 等待用户手动完成登录（定时检查 URL 变化）
4. 登录成功后 dump cookies 保存
5. 后续请求复用 cookie，无需再次交互
6. cookie 过期时提示用户 `otter auth login <provider>` 重新登录