# OpenClaw 架构与工作流程详解

> 基于源码 `D:/A/code/claude/openclaw` 与 DeepWiki 文档整理
> 版本：2026.3.x

---

## 一、项目是什么

**OpenClaw** 是一个**自托管的多渠道 AI 代理网关**（self-hosted multi-channel AI agent gateway）。

用一句话说：它是一个跑在你自己设备上的"AI 管家"，把 WhatsApp、Telegram、Discord、Slack、iMessage 等 20+ 消息平台，统一接入到 AI 大模型（Anthropic、OpenAI、Google Gemini 等），让你通过任何消息 App 和 AI 代理对话。

```
你的手机/电脑
  ↓  发消息
Telegram / WhatsApp / Slack / Discord / ...
  ↓  Channel 插件接收
OpenClaw Gateway（本地运行）
  ↓  路由 + 会话管理
AI Agent（Claude / GPT / Gemini）
  ↑  返回回复
Channel 插件发送回去
```

---

## 二、顶层目录结构

```
openclaw/
├── src/                    # TypeScript 主源码
│   ├── gateway/            # 核心网关服务器（WebSocket + HTTP）
│   ├── agents/             # AI 代理执行引擎
│   ├── channels/           # 消息渠道插件体系
│   ├── routing/            # 消息路由与会话键解析
│   ├── sessions/           # 会话状态与历史存储
│   ├── plugins/            # 插件加载与运行时
│   ├── config/             # 配置加载、验证、热重载
│   ├── context-engine/     # 上下文/记忆管理
│   ├── auto-reply/         # 自动回复调度
│   ├── security/           # 安全与授权
│   ├── cli/                # 命令行界面
│   └── ...（更多子系统见下文）
├── apps/                   # 移动端应用（iOS / Android / macOS）
├── packages/               # Monorepo 子包
├── skills/                 # AI 技能扩展（类似工具函数）
├── extensions/             # 渠道扩展
├── docs/                   # 文档
├── package.json            # 项目入口（pnpm monorepo）
├── pnpm-workspace.yaml     # Workspace 配置
└── Dockerfile              # 容器化支持
```

---

## 三、整体架构分层

OpenClaw 分为 **5 个核心层**：

```
┌─────────────────────────────────────────────┐
│         第 5 层：外部服务（模型 + Docker）    │
│   Anthropic / OpenAI / Gemini / 本地模型     │
│   Docker Sandbox（工具隔离执行）             │
├─────────────────────────────────────────────┤
│         第 4 层：工具 & 记忆子系统           │
│   Tools（分层过滤）    Memory（SQLite/QMD）  │
├─────────────────────────────────────────────┤
│         第 3 层：代理执行层（Agents）         │
│   runReplyAgent → 模型调用 → 工具执行       │
├─────────────────────────────────────────────┤
│         第 2 层：控制平面（Gateway）          │
│   WebSocket + HTTP Server（端口 18789）      │
│   路由 / 会话 / 配置 / 认证 / RPC 方法      │
├─────────────────────────────────────────────┤
│         第 1 层：消息接入层（Channels）       │
│   WhatsApp / Telegram / Discord / Slack / … │
└─────────────────────────────────────────────┘
```

---

## 四、Gateway（控制平面）详解

**位置：** `src/gateway/`

Gateway 是整个系统的大脑，运行在本地，监听端口 **18789**。

### 4.1 核心文件职责

| 文件 | 职责 |
|------|------|
| `server.impl.ts` | Gateway 主服务初始化，1200+ 行 |
| `server-http.ts` | HTTP 请求处理（OAuth、Webhook、Dashboard） |
| `server-ws-runtime.ts` | WebSocket 客户端连接管理 |
| `server-channels.ts` | 渠道生命周期管理（启动/停止/重启） |
| `server-chat.ts` | 聊天消息分发调度 |
| `server-cron.ts` | 定时任务执行 |
| `server-methods.ts` | RPC 方法注册中心（60+ 个方法） |
| `auth.ts` | 认证与授权 |
| `hooks.ts` | Webhook 集成 |
| `protocol/index.ts` | 协议类型定义（GatewayFrame、Snapshot） |

### 4.2 RPC 方法体系

Gateway 通过 WebSocket 暴露 **RPC 方法**，按领域分组：

```typescript
// src/gateway/server-methods.ts
coreGatewayHandlers = {
  ...chatHandlers,        // chat.send / chat.history / chat.abort
  ...agentHandlers,       // agent.run / agent.wait
  ...channelsHandlers,    // channels.status / channels.list
  ...configHandlers,      // config.get / config.patch
  ...cronHandlers,        // cron.add / cron.run / cron.delete
  ...sessionsHandlers,    // sessions.list / sessions.get
  ...skillsHandlers,      // skills 管理
  ...healthHandlers,      // health 探针
  // ... 共 30+ 组，60+ 个方法
}
```

常用方法举例：

| 方法 | 作用 |
|------|------|
| `chat.send` | 发消息给代理（最核心方法） |
| `chat.history` | 获取对话历史 |
| `chat.abort` | 中止正在运行的推理 |
| `agent.run` | 触发代理执行一个任务 |
| `channels.status` | 查看渠道连接状态 |
| `config.get` / `config.patch` | 读写配置 |
| `health` | 健康探针 |

---

## 五、Channels（消息渠道层）详解

**位置：** `src/channels/`

每个渠道是一个**插件（ChannelPlugin）**，实现标准接口：

```typescript
// src/channels/plugins/types.ts
type ChannelPlugin = {
  id: ChannelId
  setup?: ChannelSetupAdapter       // 初始化/配置
  status?: ChannelStatusAdapter     // 健康检查
  outbound?: ChannelOutboundAdapter // 发送消息
  messaging?: ChannelMessagingAdapter // 接收消息
  streaming?: ChannelStreamingAdapter // 流式输出
  group?: ChannelGroupAdapter       // 群组功能
  security?: ChannelSecurityAdapter // 安全策略
  pairing?: ChannelPairingAdapter   // 设备配对
  // ... 还有更多 Adapter
}
```

### 5.1 内置渠道

| 渠道 | 底层库 | 类型 |
|------|--------|------|
| WhatsApp | Baileys | 内置 |
| Telegram | grammy | 内置 |
| Discord | Carbon | 内置 |
| Slack | Bolt | 内置 |
| Signal | signal-cli | 内置 |
| iMessage | BlueBubbles HTTP API | 插件 |
| Google Chat | HTTP API | 插件 |
| Mattermost | HTTP API | 插件 |
| LINE | HTTP API | 扩展 |
| Matrix | HTTP API | 扩展 |

### 5.2 渠道管理器

`server-channels.ts` 中的 **ChannelManager** 负责：
- 启动/停止每个渠道账号
- 维护渠道健康状态（含退避重试）
- 广播渠道状态变更事件
- 快照渠道账号信息（ChannelAccountSnapshot）

---

## 六、Agents（代理执行层）详解

**位置：** `src/agents/`

代理是实际和 AI 模型交互的执行单元。

### 6.1 代理执行链

```
runReplyAgent()               ← 入口，管理"正在输入"指示器
  ↓
runAgentTurnWithFallback()    ← 实现模型故障切换
  ↓
runEmbeddedPiAgent()          ← 认证重试逻辑
  ↓
runEmbeddedAttempt()          ← 单次推理执行（调用 Pi SDK）
```

### 6.2 关键文件

| 文件 | 职责 |
|------|------|
| `agent-scope.ts` | 代理配置解析与查找 |
| `acp-spawn.ts` | 以子进程方式启动外部代理 |
| `model-catalog.ts` | 可用模型目录管理 |
| `model-selection.ts` | 模型选择与 fallback 逻辑 |
| `model-auth.ts` | 模型 API Key 认证 |
| `workspace.ts` | 代理工作区路径管理 |
| `timeout.ts` | 代理执行超时控制 |
| `skills/` | 技能（工具）集成 |

### 6.3 多代理路由（Bindings）

可配置多个代理，通过 `bindings` 规则路由不同渠道的消息：

```json
{
  "bindings": [
    { "agentId": "main",    "match": { "channel": "whatsapp" } },
    { "agentId": "work",    "match": { "channel": "telegram" } },
    { "agentId": "coding",  "match": { "channel": "slack", "accountId": "dev" } }
  ]
}
```

---

## 七、完整消息处理工作流

### 7.1 入站消息（收到消息）

```
用户在 Telegram 发送: "帮我写一个排序算法"
        │
        ▼
① Telegram Channel 插件接收原始消息
        │  parseInboundMessage()
        ▼
② Gateway 解析会话键（SessionKey）
        │  resolveAgentRoute() — 查找匹配的 binding
        ▼
③ SessionManager 加载/创建会话文件
        │  读取历史对话记录（transcript）
        ▼
④ 调用 chat.send RPC 方法
        │  验证权限、解析附件
        ▼
⑤ runReplyAgent() 启动代理执行
        │
        ▼
⑥ 加载会话 transcript → 选择模型 → 准备上下文
        │
        ▼
⑦ 调用 LLM API（Claude / GPT / Gemini）
        │  流式输出
        ▼
⑧ 解析响应，按需执行 Tools（工具调用）
        │
        ▼
⑨ 收集最终回复，写入 transcript
        │
        ▼
⑩ Channel 发送适配器将回复发回 Telegram
        │
        ▼
用户收到回复: "以下是快速排序算法..."
```

### 7.2 工具调用流程

当 AI 决定调用工具时：

```
LLM 输出 tool_call 指令
  ↓
Tool Policy 过滤（global → agent → group → sandbox）
  ↓
若工具需要代码执行 → Docker Sandbox 隔离运行
  ↓
工具返回结果 → 注入到上下文
  ↓
LLM 继续推理，生成最终回复
```

---

## 八、Session（会话）系统

**位置：** `src/sessions/` 和 `src/routing/`

### 8.1 会话键（SessionKey）结构

会话由以下维度唯一确定：

```
SessionKey = {
  agentId     // 哪个代理（如 "main"）
  scope       // 隔离范围（main/dm/group/thread/...）
  channel     // 渠道类型（telegram/discord/...）
  accountId   // 渠道账号 ID
  userId      // 用户 ID（可选）
  threadId    // 线程 ID（可选，针对 thread scope）
}
```

### 8.2 会话隔离模式（dmScope）

| 模式 | 含义 |
|------|------|
| `per-channel-peer` | 每个用户独立会话（默认，最隔离） |
| `per-channel` | 同渠道账号共享会话 |
| `global` | 所有渠道共用一个会话 |

### 8.3 会话存储

- 以 JSON 文件形式存储在 `~/.openclaw/sessions.json`
- 每个会话包含：完整对话历史（transcript）、元数据、模型状态
- 支持自动重置（按时间或消息数量触发）

---

## 九、Configuration（配置）系统

**位置：** `src/config/`

### 9.1 配置文件

默认路径：`~/.openclaw/openclaw.json`（JSON5 格式，支持注释）

### 9.2 主要配置段

```json5
{
  "gateway": {
    "port": 18789,
    "bind": "loopback",   // 仅本地访问
    "auth": { "mode": "token" },
    "reload": "hybrid"    // 热重载模式
  },
  "agents": [
    { "id": "main", "workspace": "~/.openclaw/workspace" }
  ],
  "channels": {
    "telegram": { "token": "YOUR_BOT_TOKEN" },
    "discord":  { "token": "YOUR_DISCORD_TOKEN" }
  },
  "models": {
    "providers": {
      "anthropic": { "apiKey": "sk-ant-..." }
    },
    "default": "claude-sonnet-4-6",
    "fallback": ["claude-haiku-4-5", "gpt-4o-mini"]  // 模型故障切换
  },
  "tools": {
    "policy": "permissive",
    "sandbox": { "engine": "docker" }
  },
  "memory": {
    "backend": "builtin"  // SQLite 或 qmd
  },
  "bindings": [
    { "agentId": "main", "match": { "channel": "telegram" } }
  ]
}
```

### 9.3 热重载流程

```
文件变更（inotify/chokidar 监听）
  ↓
读取文件 → 解析 JSON5 → 解析 includes
  ↓
Zod Schema 验证
  ↓
注入 Secrets（环境变量替换）
  ↓
检查 reload 模式：
  • hybrid  → 部分配置热更新，影响大的重启
  • hot     → 全部热更新
  • restart → 总是重启
  • off     → 不自动重载
```

---

## 十、Memory（记忆）子系统

**位置：** `src/context-engine/` 和 `src/memory/`

### 10.1 两种后端

| 后端 | 实现 | 特点 |
|------|------|------|
| **builtin** | SQLite + FTS5 + sqlite-vec | 内嵌，无需额外进程，支持 BM25 + 向量检索 |
| **qmd** | 外部进程 + MCP 协议 | 通过 mcporter 连接，功能更强 |

### 10.2 混合搜索排序

builtin 后端将 BM25 全文检索分数与向量相似度合并，使用 **MMR + 时间衰减** 算法：

```
最终分数 = α × BM25分数 + β × 向量相似度 - γ × 时间衰减系数
```

---

## 十一、Plugin（插件）系统

**位置：** `src/plugins/`

### 11.1 插件发现与加载

```
plugins.load.paths 配置的路径
  ↓
discovery.ts 扫描目录
  ↓
loader.ts 动态加载插件模块
  ↓
registry.ts 注册插件
```

### 11.2 插件运行时（PluginRuntime）

每个插件可以访问沙箱化的运行时：

```typescript
type PluginRuntime = {
  config:      RuntimeConfig      // 读写配置
  channel:     RuntimeChannel     // 发送消息
  subagent:    RuntimeSubagent    // 调用子代理
  system:      RuntimeSystem      // 系统信息
  media:       RuntimeMedia       // 媒体处理
  tts:         RuntimeTTS         // 文字转语音
  tools:       RuntimeTools       // 工具调用
  events:      RuntimeEvents      // 事件发布订阅
  logging:     RuntimeLogging     // 日志
  state:       RuntimeState       // 状态持久化
  modelAuth:   RuntimeModelAuth   // 模型认证
}
```

### 11.3 SDK 分层

```
openclaw/plugin-sdk/core        → 通用核心工具
openclaw/plugin-sdk/telegram    → Telegram 专用 API
openclaw/plugin-sdk/discord     → Discord 专用 API
openclaw/plugin-sdk/slack       → Slack 专用 API
```

---

## 十二、服务启动流程

### 12.1 入口链

```
openclaw.mjs（CLI 入口）
  ↓
src/entry.ts（Node.js 入口点）
  ↓
src/index.ts（CLI 初始化）
  ↓
src/cli/program.ts（Commander.js 命令树）
  ↓
openclaw gateway start
  ↓
src/gateway/server.impl.ts（Gateway 主服务）
```

### 12.2 Gateway 启动顺序

```
1. 加载并验证配置（Zod schema）
2. 初始化日志系统
3. 初始化认证与限流
4. 加载插件（发现 → 注册 → 初始化）
5. 启动渠道管理器（ChannelManager）
6. 创建代理运行时（AgentRegistry）
7. 启动 HTTP 服务器（Dashboard + OAuth + Webhooks）
8. 启动 WebSocket 服务器（RPC 接口）
9. 启动健康监控（Heartbeat）
10. 启动定时任务调度器（Cron）
11. 注册信号处理（SIGTERM/SIGINT → 优雅关闭）
12. 就绪 → 写入 PID 文件，开始接受连接
```

### 12.3 持续运行的循环

| 循环 | 作用 |
|------|------|
| 心跳监控 | 定期检查系统健康 |
| 渠道健康检查 | 监测各渠道连接状态 |
| 配置变更轮询 | 检测配置文件修改 |
| Cron 任务调度 | 定时触发任务 |
| 消息队列处理 | 处理排队的出站消息 |

---

## 十三、安全体系

**位置：** `src/security/`、`src/secrets/`、`src/gateway/auth*.ts`

| 功能 | 实现 |
|------|------|
| 认证模式 | Token / Password / 设备配对 |
| 授权 | RBAC（operator / node / viewer 三角色） |
| 限流 | 认证失败退避（auth-rate-limit.ts） |
| 传输安全 | TLS/HTTPS 支持 |
| 密钥管理 | secrets 隔离存储，支持环境变量注入 |
| 跨域保护 | CORS / CSRF / CSP Headers |

---

## 十四、CLI 命令体系

```
openclaw
├── gateway
│   ├── start / stop / restart
│   ├── status
│   ├── install / uninstall   ← 注册为系统服务
│   └── logs
├── channels
│   ├── list / status
│   └── connect / disconnect
├── agents
│   ├── list / status
│   └── run
├── models
│   ├── list / auth
│   └── test
├── memory
│   ├── search
│   └── stats
├── cron
│   ├── list / add / delete / run
├── hooks
├── skills
├── plugins
├── nodes                     ← 远程节点管理（手机等）
├── security
│   └── key / role
├── doctor                    ← 诊断与修复工具
├── dashboard                 ← 打开浏览器控制台
└── onboard                   ← 新手引导向导
```

---

## 十五、关键协议类型

```typescript
// 客户端发送给 Gateway 的帧
type GatewayFrame = {
  id?: string       // 请求 ID（用于匹配响应）
  method: string    // RPC 方法名，如 "chat.send"
  params?: unknown  // 方法参数
}

// Gateway 推送给客户端的事件帧
type EventFrame = {
  type: string      // 事件类型，如 "chat.delta"
  payload?: unknown
}

// 连接建立后的初始快照
type Snapshot = {
  presence:       SystemPresence[]  // 各渠道在线状态
  health:         HealthSummary     // 系统健康摘要
  configPath:     string            // 配置文件路径
  stateDir:       string            // 状态目录
  sessionDefaults: SessionDefaults  // 会话默认配置
  authMode:       AuthMode          // 当前认证模式
}
```

---

## 十六、状态目录结构

```
~/.openclaw/                     ← 默认状态目录
├── openclaw.json                ← 主配置文件（JSON5）
├── sessions.json                ← 会话索引
├── credentials/                 ← 渠道凭证（加密存储）
├── workspace/                   ← 默认代理工作区
│   └── <agent-id>/
│       └── transcripts/         ← 对话历史文件
├── plugins/                     ← 已安装插件
└── logs/ → /tmp/openclaw/       ← 运行日志

~/.openclaw-dev/                 ← 开发环境（--dev 标志）
~/.openclaw-<name>/              ← 自定义 Profile（--profile <name>）
```

---

## 十七、移动端支持

OpenClaw 还包含原生移动 App，作为**控制节点**（不运行 AI，只是 Gateway 的客户端）：

| 平台 | 技术栈 |
|------|--------|
| iOS | Swift 6.0 + SwiftUI，XcodeGen 生成项目 |
| Android | Kotlin + Jetpack Compose + Material 3 |
| macOS | 签名 .app 包，通过 `pnpm mac:package` 构建 |

移动 App 通过 WebSocket 连接本地或远程运行的 Gateway，使用与 CLI 相同的 RPC 协议。

---

## 十八、总结：核心设计思想

| 设计决策 | 原因 |
|----------|------|
| **本地部署** | 数据不经过第三方服务器，保护隐私 |
| **渠道插件化** | 轻松扩展新平台，不改动核心代码 |
| **模型 fallback** | 单个模型 API 故障时自动切换，保证可用性 |
| **会话持久化** | 跨重启保持对话上下文 |
| **配置热重载** | 修改配置无需重启服务 |
| **JSON5 配置** | 支持注释，方便人工编辑 |
| **Zod 验证** | 配置错误时立即报告，不会静默失败 |
| **Docker 沙箱** | 工具执行隔离，防止恶意代码 |
| **RPC over WebSocket** | 实时双向通信，支持流式响应 |

---

*文档生成时间：2026-03-14*
*数据来源：源码分析 + DeepWiki https://deepwiki.com/openclaw/openclaw/*
