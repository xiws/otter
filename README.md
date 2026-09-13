# otter

AI Web 批量自动化 CLI 工具。通过浏览器自动化或逆向 Web API 两种策略，将本地文件与 Prompt 批量发送给 ChatGPT、Gemini、DeepSeek 等 AI 大模型。

## 快速开始

```bash
# 构建
make build

# 查看帮助
./build/otter --help

# 1. 配置 DeepSeek 账号密码
./build/otter config set deepseek.account "你的手机号或邮箱"
./build/otter config set deepseek.password "你的密码"

# 2. 登录 DeepSeek（会自动保存 token）
./build/otter auth login deepseek

# 3. 发送第一条消息
./build/otter dep "你好，世界"

# 或一步到位：配置好账号密码后，直接发消息会自动登录
./build/otter dep "快速问答"

# 创建并选择会话
./build/otter session new deepseek --title "代码审查"
./build/otter session select deepseek <session-id>

# 发送消息并上传附件
./build/otter dep -f ./code.py "解释这段代码"

# ChatGPT / Gemini 浏览器登录（首次需要）
./build/otter auth login gpt
./build/otter auth login gem

# 发送消息到 ChatGPT / Gemini
./build/otter gpt "你好，世界"
./build/otter gem -f ./report.pdf "帮我润色这份报告"

# 查看配置和登录状态
./build/otter config show
./build/otter auth check
```

## 命令概览

| 命令 | 用途 | 状态 |
|---|---|---|
| `otter gpt <prompt>` | 向 ChatGPT 发送消息 | ✅ Phase 2 |
| `otter gem <prompt>` | 向 Gemini 发送消息 | ✅ Phase 2 |
| `otter dep <prompt>` | 向 DeepSeek 发送消息 | ✅ Phase 1 |
| `otter auth login <provider>` | 登录 AI 平台 | ✅ |
| `otter auth check [provider]` | 检查登录状态 | ✅ |
| `otter session new` | 创建新会话 | ✅ |
| `otter session list` | 列举会话 | ✅ |
| `otter session select` | 选择活动会话 | ✅ |
| `otter session show` | 查看会话详情 | ✅ |
| `otter session rm` | 删除会话 | ✅ |
| `otter config show` | 查看配置 | ✅ |
| `otter config set` | 设置配置项 | ✅ |

## 认证方式

### DeepSeek（Direct API）

DeepSeek 使用**账号密码直接登录**（自身认证），无需手动找 token：

1. 配置账号密码：`otter config set deepseek.account <账号>`
2. 执行登录：`otter auth login deepseek`
3. 登录成功后 token 自动保存到配置文件，后续复用

或者直接发送消息时会**自动检测**并登录：
```bash
otter config set deepseek.account "151****0000"
otter config set deepseek.password "mypassword"
otter dep "你好"   # 自动登录并发送
```

也可以通过环境变量覆盖密码：`export OTTER_DEEPSEEK_PASSWORD=xxx`

### ChatGPT（混合模式：浏览器登录 + Direct API）

ChatGPT 使用**Playwright 浏览器自动化**完成 Google OAuth 登录，
登录后自动提取 session token，后续消息通过 Direct API 发送（无需浏览器）。

```bash
# 1. 首次登录（会自动打开浏览器窗口）
otter auth login gpt

# 2. 发送消息（后续无需浏览器）
otter gpt "你好，世界"
otter gpt -f ./report.pdf "帮我润色这份报告"
```

### Gemini（全浏览器模式）

Gemini 使用**Playwright 浏览器自动化**完成登录和消息收发。
所有操作通过浏览器执行，cookie 会被持久化以复用登录状态。

```bash
# 1. 首次登录（会自动打开浏览器窗口）
otter auth login gem

# 2. 发送消息（浏览器自动处理）
otter gem "你好，世界"
otter gem -f ./code.py "解释这段代码"
```

### 前提条件

ChatGPT 和 Gemini 依赖 Playwright 浏览器自动化。首次使用时会自动安装：

```bash
# 或手动安装
cd otter && go run github.com/mxschmitt/playwright-go/cmd/playwright install --driver
```

## 项目结构

```
otter/
├── cmd/cli/main.go          # 入口
├── pkg/
│   ├── cli/                 # CLI 命令（cobra）
│   ├── config/              # 配置读写
│   ├── session/             # 会话管理
│   ├── provider/            # Provider 接口 + 各平台实现
│   │   ├── provider.go      # 接口定义
│   │   ├── deepseek.go      # DeepSeek 实现（Direct API）
│   │   ├── chatgpt.go       # ChatGPT 实现（Hybrid）
│   │   └── gemini.go        # Gemini 实现（Browser）
│   ├── api/                 # HTTP 客户端 + 各平台 API
│   │   ├── client.go        # 通用 HTTP 客户端
│   │   ├── deepseek_api.go  # DeepSeek Direct API
│   │   └── chatgpt_api.go   # ChatGPT Direct API
│   ├── browser/
│   │   └── driver.go        # Playwright 浏览器驱动
│   ├── pow/                 # DeepSeek PoW 求解（内嵌 WASM）
│   └── output/              # 输出格式化
├── docs/main.md             # 完整设计文档
├── Makefile
└── README.md
```

### 关于 pkg/pow

DeepSeek 对对话和文件上传接口加了工作量证明（PoW）校验。求解算法由 DeepSeek
前端分发的 WebAssembly 模块实现，算法细节未公开，因此该目录内嵌了这个模块
（`sha3_wasm.wasm`，26612 字节），用纯 Go 的
[wazero](https://github.com/tetratelabs/wazero) 运行时调用，无 CGO 依赖。

```
sha3_wasm.wasm  SHA256 b3fca8cc072c1defbd60c02266a8e48bd307a1804aaff4314900aea720e72f7d
```

## 开发

```bash
make build   # 编译
make test    # 运行测试
make vet     # 代码检查
make clean   # 清理构建产物
```

## 实现进度

- **Phase 1** ✅ — CLI 骨架 + DeepSeek Direct API (MVP)
  - DeepSeek 账号密码登录（手机号走 `mobile`、邮箱走 `email`）
  - PoW 挑战求解（内嵌官方 WASM + wazero，无 CGO）
  - 会话管理（创建/选择/列举/查看/删除），本地会话与平台 UUID 自动映射
  - 多轮对话上下文（`parent_message_id`）
  - 配置管理
  - 文件附件上传（含异步处理状态等待）
  - 流式输出
- **Phase 2** ✅ — 浏览器自动化（Playwright 集成）
  - Playwright 浏览器驱动（BrowserDriver）
  - ChatGPT 混合模式：Playwright 浏览器登录 + Direct API 消息收发
  - Gemini 全浏览器自动化模式
  - Cookie 持久化与复用
  - `otter auth login gpt` / `otter auth login gem`
  - `otter gpt` / `otter gem` 发送命令
  - 文件附件上传支持
- **Phase 3** 📋 — 增强与优化

详见 [docs/main.md](docs/main.md)。