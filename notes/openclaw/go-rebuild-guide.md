# 用 Go 从零复现 OpenClaw — 学习指南

> 目标：用 Go 复现 OpenClaw 的完整架构，从单 Telegram 渠道出发，逐步扩展到多渠道、多代理、工具调用、记忆系统。
> 每一阶段都对应架构中的真实模块，边写边学核心工程思想。

---

## 总体路线图

```
Phase 1  最小可用版本
         Telegram Bot → 直接调用 AI → 回复
         学习：HTTP 轮询/Webhook、流式 API、goroutine 基础

Phase 2  会话管理
         多用户独立对话历史，内存 → 文件持久化
         学习：并发安全 Map、JSON 序列化、文件 IO

Phase 3  Gateway 服务器
         WebSocket RPC 层、HTTP Dashboard
         学习：WebSocket、RPC 协议设计、广播/订阅

Phase 4  配置系统
         YAML/JSON5 配置、热重载、Zod-like 验证
         学习：fsnotify、结构化配置、校验设计

Phase 5  Channel 抽象
         统一接口隔离 Telegram 细节，为扩展做准备
         学习：Go interface 设计、适配器模式

Phase 6  Agent 抽象 + 多模型支持
         模型 fallback、超时控制、中止机制
         学习：context.Context、errgroup、策略模式

Phase 7  Tools（工具调用）
         AI 函数调用、工具注册中心、权限过滤
         学习：JSON Schema、动态分发、沙箱思想

Phase 8  Memory（记忆系统）
         SQLite FTS5 全文检索 + 向量搜索
         学习：CGO/纯 Go SQLite、BM25、混合检索

Phase 9  多渠道扩展
         接入 Discord / Slack，验证抽象层设计
         学习：插件注册、动态加载思想
```

---

## Phase 1 — 最小可用版本

### 目标

```
Telegram 发消息 → 调用 Claude/OpenAI → 流式回复
```

### 学习目标
- Telegram Bot API 的两种接收模式（Long Polling vs Webhook）
- 调用 Anthropic/OpenAI API，处理流式 SSE 响应
- 基础 goroutine + channel 并发

### 目录结构

```
goclaw/
├── go.mod
├── main.go
├── internal/
│   ├── telegram/
│   │   ├── bot.go          # Bot 初始化、消息循环
│   │   └── types.go        # Telegram API 结构体
│   └── ai/
│       ├── client.go       # AI 客户端接口
│       ├── anthropic.go    # Anthropic 实现
│       └── openai.go       # OpenAI 实现
└── config.yaml
```

### 关键代码骨架

```go
// internal/telegram/bot.go

type Bot struct {
    token   string
    apiBase string
    handler MessageHandler
}

type MessageHandler func(ctx context.Context, msg *Message) (string, error)

func (b *Bot) StartPolling(ctx context.Context) error {
    offset := 0
    for {
        select {
        case <-ctx.Done():
            return nil
        default:
        }
        updates, err := b.getUpdates(ctx, offset)
        if err != nil {
            // 退避重试，不要 panic
            time.Sleep(3 * time.Second)
            continue
        }
        for _, u := range updates {
            offset = u.UpdateID + 1
            go b.handleUpdate(ctx, u) // 每条消息独立 goroutine
        }
    }
}
```

```go
// internal/ai/client.go

type Client interface {
    // 流式生成，通过 channel 逐块返回文本
    StreamChat(ctx context.Context, messages []Message) (<-chan string, error)
}

type Message struct {
    Role    string // "user" | "assistant" | "system"
    Content string
}
```

### 工程细节要点

**流式回复的正确处理方式：**
OpenClaw 用流式输出 + "正在输入"指示器（typing indicator）。你需要：
1. 先发送一条空消息（或"正在思考…"），获取 `message_id`
2. AI 流式输出时，累积文本后 **Edit** 那条消息（避免刷屏）
3. 有节流：不要每个 token 都编辑，每 300ms 或每 50 字符编辑一次

```go
// 节流编辑示例
func streamToTelegram(ctx context.Context, bot *Bot, chatID int64, stream <-chan string) {
    var buf strings.Builder
    ticker := time.NewTicker(300 * time.Millisecond)
    defer ticker.Stop()
    msgID, _ := bot.SendMessage(chatID, "…")

    for {
        select {
        case chunk, ok := <-stream:
            if !ok {
                bot.EditMessage(chatID, msgID, buf.String()) // 最终更新
                return
            }
            buf.WriteString(chunk)
        case <-ticker.C:
            if buf.Len() > 0 {
                bot.EditMessage(chatID, msgID, buf.String())
            }
        }
    }
}
```

**对照 OpenClaw：** 这对应 `runReplyAgent()` 中管理 typing indicator 的逻辑。

---

## Phase 2 — 会话管理

### 目标

每个用户独立的对话历史，重启后不丢失。

### 学习目标
- `sync.RWMutex` 保护并发访问的 Map
- 会话键（SessionKey）设计 —— 如何唯一标识一个对话
- JSON 持久化 + 原子写入防止文件损坏

### 目录结构新增

```
internal/
└── session/
    ├── store.go        # SessionStore 接口 + 实现
    ├── session.go      # Session 结构体、Transcript
    └── key.go          # SessionKey 解析与生成
```

### SessionKey 设计

这是 OpenClaw 架构的核心概念之一。一个会话由多个维度确定：

```go
// internal/session/key.go

type SessionKey struct {
    ChannelID string // "telegram"
    AccountID string // Bot 账号标识
    Scope     Scope  // 会话隔离级别
    PeerID    string // 用户/群组 ID
    ThreadID  string // 可选，线程 ID
    AgentID   string // 使用哪个 AI 代理
}

type Scope string

const (
    ScopeDirect  Scope = "dm"      // 私聊：每个用户独立
    ScopeGroup   Scope = "group"   // 群组：群内共享
    ScopeGlobal  Scope = "global"  // 全局共用（少见）
)

func (k SessionKey) String() string {
    return fmt.Sprintf("%s:%s:%s:%s:%s", k.ChannelID, k.AccountID, k.Scope, k.PeerID, k.AgentID)
}
```

### SessionStore 实现

```go
// internal/session/store.go

type Session struct {
    Key        SessionKey `json:"key"`
    Messages   []Message  `json:"messages"`
    CreatedAt  time.Time  `json:"created_at"`
    UpdatedAt  time.Time  `json:"updated_at"`
}

type Store interface {
    Get(key SessionKey) (*Session, error)
    Save(s *Session) error
    Delete(key SessionKey) error
}

// FileStore：每个会话一个 JSON 文件
type FileStore struct {
    dir string
    mu  sync.RWMutex
    // 内存缓存，避免每次都读磁盘
    cache map[string]*Session
}

func (s *FileStore) Save(sess *Session) error {
    data, err := json.Marshal(sess)
    if err != nil {
        return err
    }
    // 原子写入：先写临时文件，再 rename
    // rename 在同一文件系统上是原子操作，防止进程崩溃导致文件损坏
    tmp := s.pathFor(sess.Key) + ".tmp"
    if err := os.WriteFile(tmp, data, 0600); err != nil {
        return err
    }
    return os.Rename(tmp, s.pathFor(sess.Key))
}
```

**工程细节：原子写入** —— 直接 `os.WriteFile` 在写到一半时崩溃会留下损坏文件。`写tmp → rename` 利用 rename 的原子性避免这个问题。OpenClaw 的 sessions.json 同样采用此模式。

### 上下文窗口管理

AI 模型有 token 限制，历史消息不能无限增长：

```go
// 简单截断策略：保留最近 N 条 + system prompt
func (s *Session) TrimmedMessages(maxMessages int) []Message {
    msgs := s.Messages
    if len(msgs) <= maxMessages {
        return msgs
    }
    // 永远保留第一条 system message
    if msgs[0].Role == "system" {
        return append(msgs[:1], msgs[len(msgs)-maxMessages+1:]...)
    }
    return msgs[len(msgs)-maxMessages:]
}
```

---

## Phase 3 — Gateway 服务器

### 目标

构建 WebSocket + HTTP 控制平面，客户端通过 RPC 控制系统。

### 学习目标
- WebSocket 全双工通信（`gorilla/websocket` 或标准库）
- RPC 协议设计（方法名 + 参数 + 响应 ID 匹配）
- 广播/订阅模式（Hub 模式）
- HTTP 中间件链

### 目录结构新增

```
internal/
└── gateway/
    ├── server.go       # Gateway 主服务器
    ├── ws.go           # WebSocket 连接管理
    ├── hub.go          # 广播 Hub
    ├── rpc.go          # RPC 分发器
    ├── methods/        # 各 RPC 方法实现
    │   ├── chat.go
    │   ├── channels.go
    │   └── health.go
    └── protocol.go     # 协议类型定义
```

### 协议设计

```go
// internal/gateway/protocol.go

// 客户端 → 服务端：请求帧
type RequestFrame struct {
    ID     string          `json:"id"`     // 请求 ID，用于匹配响应
    Method string          `json:"method"` // 如 "chat.send"
    Params json.RawMessage `json:"params"` // 方法参数
}

// 服务端 → 客户端：响应帧
type ResponseFrame struct {
    ID    string          `json:"id"`              // 对应请求 ID
    Data  json.RawMessage `json:"data,omitempty"`  // 成功数据
    Error *RPCError       `json:"error,omitempty"` // 错误信息
}

// 服务端 → 客户端：事件帧（主动推送，无 ID）
type EventFrame struct {
    Type    string          `json:"type"`    // 如 "chat.delta"
    Payload json.RawMessage `json:"payload"`
}

type RPCError struct {
    Code    string `json:"code"`
    Message string `json:"message"`
}
```

### Hub（广播中心）

```go
// internal/gateway/hub.go

type Hub struct {
    clients    map[*Client]bool
    broadcast  chan EventFrame
    register   chan *Client
    unregister chan *Client
    mu         sync.RWMutex
}

func (h *Hub) Run() {
    for {
        select {
        case client := <-h.register:
            h.mu.Lock()
            h.clients[client] = true
            h.mu.Unlock()

        case client := <-h.unregister:
            h.mu.Lock()
            delete(h.clients, client)
            h.mu.Unlock()
            close(client.send)

        case event := <-h.broadcast:
            data, _ := json.Marshal(event)
            h.mu.RLock()
            for client := range h.clients {
                select {
                case client.send <- data:
                default:
                    // 客户端发送缓冲满，断开连接
                    close(client.send)
                    delete(h.clients, client)
                }
            }
            h.mu.RUnlock()
        }
    }
}
```

### RPC 分发器

```go
// internal/gateway/rpc.go

type Handler func(ctx context.Context, params json.RawMessage) (any, error)

type Router struct {
    handlers map[string]Handler
}

func (r *Router) Register(method string, h Handler) {
    r.handlers[method] = h
}

func (r *Router) Dispatch(ctx context.Context, frame RequestFrame) ResponseFrame {
    h, ok := r.handlers[frame.Method]
    if !ok {
        return errorResponse(frame.ID, "METHOD_NOT_FOUND", "unknown method: "+frame.Method)
    }
    result, err := h(ctx, frame.Params)
    if err != nil {
        return errorResponse(frame.ID, "INTERNAL_ERROR", err.Error())
    }
    data, _ := json.Marshal(result)
    return ResponseFrame{ID: frame.ID, Data: data}
}
```

**对照 OpenClaw：** 这完全对应 `server-methods.ts` 的 `coreGatewayHandlers` + `server-ws-runtime.ts`。

---

## Phase 4 — 配置系统

### 目标

结构化配置文件，文件变更时热重载，配置错误立即报告。

### 学习目标
- `fsnotify` 文件监听
- Go struct tag + 反射做校验
- 函数选项模式（functional options）
- 读写锁保护热重载的并发安全

### 配置结构

```go
// internal/config/types.go

type Config struct {
    Gateway  GatewayConfig           `yaml:"gateway"`
    Agents   []AgentConfig           `yaml:"agents"`
    Channels map[string]ChannelConfig `yaml:"channels"`
    Models   ModelsConfig            `yaml:"models"`
    Session  SessionConfig           `yaml:"session"`
}

type GatewayConfig struct {
    Port   int    `yaml:"port" validate:"min=1,max=65535"`
    Bind   string `yaml:"bind" validate:"oneof=loopback all"`
    Reload string `yaml:"reload" validate:"oneof=hybrid hot restart off"`
}

type AgentConfig struct {
    ID        string   `yaml:"id" validate:"required"`
    Workspace string   `yaml:"workspace"`
    Model     string   `yaml:"model"`
    Fallback  []string `yaml:"fallback"`
}

type ModelsConfig struct {
    Providers map[string]ProviderConfig `yaml:"providers"`
    Default   string                   `yaml:"default"`
}
```

### 热重载管理器

```go
// internal/config/manager.go

type Manager struct {
    path      string
    current   atomic.Pointer[Config]  // Go 1.19+，原子读写无锁
    onChange  []func(old, new *Config)
    watcher   *fsnotify.Watcher
}

func (m *Manager) Watch(ctx context.Context) error {
    for {
        select {
        case event := <-m.watcher.Events:
            if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
                m.reload()
            }
        case <-ctx.Done():
            return nil
        }
    }
}

func (m *Manager) reload() {
    newCfg, err := load(m.path)
    if err != nil {
        // 配置有错误：记录日志，继续用旧配置
        // 不要让配置文件错误导致服务崩溃
        log.Printf("config reload failed: %v (keeping old config)", err)
        return
    }
    old := m.current.Swap(newCfg)
    for _, fn := range m.onChange {
        fn(old, newCfg)
    }
}

func (m *Manager) Get() *Config {
    return m.current.Load()
}
```

**工程细节：`atomic.Pointer`** —— 比 `sync.RWMutex` 包装一个指针更简洁，读操作完全无锁，非常适合"频繁读、偶尔写"的配置场景。

---

## Phase 5 — Channel 抽象层

### 目标

用 Go interface 隔离 Telegram 实现细节，新增渠道只需实现接口。

### 学习目标
- Go interface 的组合式设计
- 适配器模式（Adapter Pattern）
- 注册表（Registry）模式

### Channel 接口设计

```go
// internal/channel/types.go

// 入站消息（标准化，与平台无关）
type InboundMessage struct {
    ChannelID string
    AccountID string
    PeerID    string    // 用户/群组 ID
    UserID    string
    Text      string
    ReplyToID string    // 引用回复
    Attachments []Attachment
    RawData   any       // 原始平台数据，备用
}

// 出站消息
type OutboundMessage struct {
    PeerID    string
    Text      string
    ReplyToID string
    ParseMode string // "markdown" | "html" | ""
}

// 渠道必须实现的核心接口
type Channel interface {
    ID() string                                          // "telegram"
    Start(ctx context.Context) error                     // 启动监听
    Stop() error                                         // 优雅关闭
    Send(ctx context.Context, msg OutboundMessage) error // 发送消息
    Edit(ctx context.Context, msgID string, text string) error
    Status() ChannelStatus
}

// 可选接口：支持能力检测（interface assertion）
type TypingIndicator interface {
    SendTyping(ctx context.Context, peerID string) error
}

type MessageDeleter interface {
    Delete(ctx context.Context, peerID string, msgID string) error
}
```

### 注册表

```go
// internal/channel/registry.go

type Factory func(cfg map[string]any, handler InboundHandler) (Channel, error)

type InboundHandler func(ctx context.Context, msg InboundMessage)

var registry = map[string]Factory{}

func Register(channelID string, f Factory) {
    registry[channelID] = f
}

func Create(channelID string, cfg map[string]any, handler InboundHandler) (Channel, error) {
    f, ok := registry[channelID]
    if !ok {
        return nil, fmt.Errorf("unknown channel: %s", channelID)
    }
    return f(cfg, handler)
}

// internal/channel/telegram/telegram.go
func init() {
    channel.Register("telegram", func(cfg map[string]any, handler channel.InboundHandler) (channel.Channel, error) {
        return NewBot(cfg, handler)
    })
}
```

**使用能力检测替代臃肿接口：**

```go
// Gateway 发送消息时的正确做法
func sendWithTyping(ctx context.Context, ch channel.Channel, peerID string, text string) {
    // 如果渠道支持"正在输入"，就发
    if ti, ok := ch.(channel.TypingIndicator); ok {
        ti.SendTyping(ctx, peerID)
    }
    ch.Send(ctx, channel.OutboundMessage{PeerID: peerID, Text: text})
}
```

**对照 OpenClaw：** 这完全对应 `ChannelPlugin` 里的多个可选 Adapter（`TypingAdapter`、`StreamingAdapter` 等）。

---

## Phase 6 — Agent 抽象 + 多模型 Fallback

### 目标

独立的 Agent 执行单元，模型故障时自动切换，支持中止。

### 学习目标
- `context.Context` 传递取消信号
- `errgroup` 管理并发任务组
- 策略模式（Strategy Pattern）实现 fallback
- `context.WithCancel` 实现中止

### Agent 执行链

```go
// internal/agent/runner.go

// 对应 OpenClaw 的 runReplyAgent
func (a *Agent) RunReply(ctx context.Context, sess *session.Session, input string) error {
    // 1. 发送 typing indicator（通过 channel 接口）
    a.channel.SendTyping(ctx, sess.Key.PeerID)

    // 2. 添加用户消息到会话
    sess.AddMessage(session.Message{Role: "user", Content: input})

    // 3. 带 fallback 执行推理
    reply, err := a.runWithFallback(ctx, sess)
    if err != nil {
        return err
    }

    // 4. 保存代理回复到会话
    sess.AddMessage(session.Message{Role: "assistant", Content: reply})
    a.store.Save(sess)

    return nil
}

// 对应 runAgentTurnWithFallback
func (a *Agent) runWithFallback(ctx context.Context, sess *session.Session) (string, error) {
    models := append([]string{a.cfg.Model}, a.cfg.Fallback...)
    var lastErr error
    for _, modelID := range models {
        client, err := a.modelRegistry.Get(modelID)
        if err != nil {
            lastErr = err
            continue
        }
        result, err := a.runAttempt(ctx, client, sess)
        if err == nil {
            return result, nil
        }
        // 判断是否值得重试（限流/服务不可用才重试，参数错误不用）
        if !isRetryable(err) {
            return "", err
        }
        lastErr = err
        log.Printf("model %s failed (%v), trying next...", modelID, err)
    }
    return "", fmt.Errorf("all models failed, last error: %w", lastErr)
}
```

### 中止机制

```go
// internal/agent/abort.go

type AbortRegistry struct {
    mu      sync.Mutex
    cancels map[string]context.CancelFunc // runID → cancel
}

func (r *AbortRegistry) Register(runID string) (context.Context, context.CancelFunc) {
    ctx, cancel := context.WithCancel(context.Background())
    r.mu.Lock()
    r.cancels[runID] = cancel
    r.mu.Unlock()
    return ctx, cancel
}

func (r *AbortRegistry) Abort(runID string) bool {
    r.mu.Lock()
    cancel, ok := r.cancels[runID]
    r.mu.Unlock()
    if ok {
        cancel()
    }
    return ok
}
```

对应 RPC 方法 `chat.abort`：

```go
// internal/gateway/methods/chat.go

func (h *ChatHandler) Abort(ctx context.Context, params json.RawMessage) (any, error) {
    var p struct{ RunID string `json:"run_id"` }
    json.Unmarshal(params, &p)
    aborted := h.abortReg.Abort(p.RunID)
    return map[string]bool{"aborted": aborted}, nil
}
```

---

## Phase 7 — Tools（工具调用）

### 目标

AI 可以调用注册的工具函数（搜索网络、读写文件、执行代码等）。

### 学习目标
- JSON Schema 生成（用 Go struct tag 生成）
- 工具调用循环（Tool Use Loop）
- 权限过滤层
- 沙箱隔离思路（`os/exec` + 超时 Context）

### 工具注册

```go
// internal/tools/registry.go

type ToolInput struct {
    // 由 JSON Schema 描述，运行时通过 json.RawMessage 传入
}

type Tool struct {
    Name        string
    Description string
    InputSchema  map[string]any  // JSON Schema
    Execute      func(ctx context.Context, input json.RawMessage) (string, error)
}

type Registry struct {
    tools map[string]*Tool
}

func (r *Registry) Register(t *Tool) {
    r.tools[t.Name] = t
}

// 导出给 AI 的工具描述列表
func (r *Registry) Definitions() []map[string]any {
    var defs []map[string]any
    for _, t := range r.tools {
        defs = append(defs, map[string]any{
            "name":         t.Name,
            "description":  t.Description,
            "input_schema": t.InputSchema,
        })
    }
    return defs
}
```

### 工具调用循环

```go
// internal/agent/tool_loop.go

// AI 模型返回 tool_use 时，执行工具并继续推理
func (a *Agent) runToolLoop(ctx context.Context, messages []ai.Message) (string, error) {
    for {
        resp, err := a.client.Chat(ctx, messages, a.tools.Definitions())
        if err != nil {
            return "", err
        }

        // 纯文本回复 → 结束
        if resp.Type == "text" {
            return resp.Text, nil
        }

        // 工具调用 → 执行工具 → 加入上下文 → 继续循环
        if resp.Type == "tool_use" {
            toolResult, err := a.tools.Execute(ctx, resp.ToolName, resp.ToolInput)
            if err != nil {
                toolResult = fmt.Sprintf("error: %v", err)
            }
            // 把工具结果加入消息历史，让 AI 继续推理
            messages = append(messages,
                ai.Message{Role: "assistant", Content: resp.Raw},  // AI 的 tool_use
                ai.Message{Role: "user", Content: toolResult},     // 工具结果
            )
            continue
        }

        return "", fmt.Errorf("unexpected response type: %s", resp.Type)
    }
}
```

**工程细节：工具调用循环** —— 这个循环是 Agent 的核心，AI 不是一次回答，而是可能多轮"思考→用工具→看结果→继续思考"。循环直到 AI 输出纯文本为止。

---

## Phase 8 — Memory（记忆系统）

### 目标

长期记忆：对话历史之外，存储可检索的记忆条目（类似"笔记本"）。

### 学习目标
- CGO 调用 SQLite（`modernc.org/sqlite` 纯 Go 版）
- FTS5 全文检索（BM25 排序）
- `sqlite-vec` 向量扩展
- 混合检索排序

### SQLite 建表

```sql
-- 全文检索表（FTS5 内置 BM25）
CREATE VIRTUAL TABLE memories_fts USING fts5(
    content,
    metadata UNINDEXED,
    created_at UNINDEXED
);

-- 向量表（sqlite-vec 扩展）
CREATE VIRTUAL TABLE memories_vec USING vec0(
    id INTEGER PRIMARY KEY,
    embedding FLOAT[1536]   -- OpenAI embedding 维度
);
```

### 混合搜索

```go
// internal/memory/store.go

type SearchResult struct {
    Content   string
    Score     float64
    CreatedAt time.Time
}

func (s *Store) Search(ctx context.Context, query string, embedding []float32, limit int) ([]SearchResult, error) {
    // BM25 全文检索
    bm25Results := s.searchBM25(query, limit*2)

    // 向量相似度搜索
    vecResults := s.searchVector(embedding, limit*2)

    // 混合排序（MMR + 时间衰减）
    // 对照 OpenClaw: α×BM25 + β×向量 - γ×时间衰减
    return mergeResults(bm25Results, vecResults, limit), nil
}

func timeDecay(t time.Time) float64 {
    age := time.Since(t).Hours() / 24 // 天数
    return math.Exp(-0.1 * age)       // 指数衰减
}
```

---

## Phase 9 — 多渠道扩展

### 目标

接入 Discord，验证 Channel 抽象层是否真的够用。

### 关键验证点

```
实现 Discord Channel 时，你不应该改动 Gateway、Agent、Session 任何代码。
如果需要改动，说明抽象层设计有问题，需要重构。
```

### Discord 实现骨架

```go
// internal/channel/discord/discord.go

import "github.com/bwmarrin/discordgo"

type Discord struct {
    session *discordgo.Session
    handler channel.InboundHandler
    cfg     *Config
}

func (d *Discord) ID() string { return "discord" }

func (d *Discord) Start(ctx context.Context) error {
    d.session.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
        if m.Author.ID == s.State.User.ID { return } // 忽略自己的消息
        d.handler(ctx, channel.InboundMessage{
            ChannelID: "discord",
            AccountID: d.cfg.AccountID,
            PeerID:    m.ChannelID,
            UserID:    m.Author.ID,
            Text:      m.Content,
        })
    })
    return d.session.Open()
}

func (d *Discord) Send(ctx context.Context, msg channel.OutboundMessage) error {
    _, err := d.session.ChannelMessageSend(msg.PeerID, msg.Text)
    return err
}

// 在 init() 中注册
func init() {
    channel.Register("discord", func(cfg map[string]any, handler channel.InboundHandler) (channel.Channel, error) {
        return New(cfg, handler)
    })
}
```

---

## 推荐 Go 依赖清单

```
核心依赖：
github.com/gorilla/websocket      WebSocket 服务器
gopkg.in/yaml.v3                  YAML 配置解析
github.com/fsnotify/fsnotify      配置文件热监听
go.uber.org/zap                   结构化日志
github.com/spf13/cobra            CLI 框架

AI 客户端：
（OpenRouter 使用标准库 net/http，无需第三方 SDK）
# 通过 OpenRouter 调用 Claude：https://openrouter.ai/keys

渠道：
github.com/go-telegram-bot-api/telegram-bot-api/v5  Telegram
github.com/bwmarrin/discordgo                       Discord
github.com/slack-go/slack                           Slack

数据库：
modernc.org/sqlite                纯 Go SQLite（无 CGO 依赖）

测试：
github.com/stretchr/testify       断言库
```

---

## 项目最终目录结构（Phase 9 后）

```
goclaw/
├── main.go
├── go.mod
├── go.sum
├── config.yaml
│
├── internal/
│   ├── ai/
│   │   ├── types.go            # Message、Response 等通用类型
│   │   ├── client.go           # Client interface
│   │   ├── anthropic/          # Anthropic 实现
│   │   └── openai/             # OpenAI 实现
│   │
│   ├── channel/
│   │   ├── types.go            # InboundMessage、OutboundMessage、Channel interface
│   │   ├── registry.go         # 渠道注册表
│   │   ├── manager.go          # 渠道生命周期管理
│   │   ├── telegram/           # Telegram 实现
│   │   ├── discord/            # Discord 实现
│   │   └── slack/              # Slack 实现
│   │
│   ├── session/
│   │   ├── key.go              # SessionKey
│   │   ├── session.go          # Session 结构
│   │   └── store.go            # Store interface + FileStore
│   │
│   ├── agent/
│   │   ├── agent.go            # Agent 主结构
│   │   ├── runner.go           # runReplyAgent, runWithFallback
│   │   ├── abort.go            # AbortRegistry
│   │   ├── tool_loop.go        # 工具调用循环
│   │   └── registry.go         # Agent 注册表（多 agent 支持）
│   │
│   ├── tools/
│   │   ├── registry.go         # 工具注册
│   │   ├── executor.go         # 工具执行 + 权限过滤
│   │   └── builtin/            # 内置工具（文件读写、搜索等）
│   │
│   ├── memory/
│   │   ├── store.go            # Memory Store interface
│   │   ├── sqlite.go           # SQLite + FTS5 实现
│   │   └── search.go           # 混合检索算法
│   │
│   ├── config/
│   │   ├── types.go            # 配置结构体
│   │   ├── loader.go           # 加载 + 验证
│   │   └── manager.go          # 热重载管理
│   │
│   ├── gateway/
│   │   ├── server.go           # Gateway 主服务
│   │   ├── ws.go               # WebSocket 处理
│   │   ├── hub.go              # 广播 Hub
│   │   ├── rpc.go              # RPC 路由分发
│   │   ├── protocol.go         # 协议类型
│   │   └── methods/            # RPC 方法实现
│   │       ├── chat.go
│   │       ├── channels.go
│   │       ├── agents.go
│   │       └── health.go
│   │
│   └── routing/
│       ├── resolver.go         # resolveAgentRoute
│       └── bindings.go         # Binding 规则匹配
│
└── cmd/
    └── goclaw/
        └── main.go             # CLI 入口（cobra）
```

---

## 每阶段学到的核心工程思想

| Phase | 工程思想 | OpenClaw 对应模块 |
|-------|----------|-------------------|
| 1 | 流式 API、goroutine 生命周期、退避重试 | `runEmbeddedAttempt`, typing indicator |
| 2 | 原子文件写入、并发安全 Map、会话键设计 | `sessions/`, `session-key.ts` |
| 3 | RPC 协议设计、WebSocket Hub、中间件链 | `gateway/`, `server-ws-runtime.ts` |
| 4 | 配置热重载、`atomic.Pointer`、Zod-like 验证 | `config/`, fsnotify |
| 5 | Interface + 能力检测、注册表模式、适配器模式 | `channels/plugins/types.ts` |
| 6 | Context 取消传播、Fallback 策略链、AbortRegistry | `runAgentTurnWithFallback`, `acp-spawn.ts` |
| 7 | 工具调用循环、JSON Schema、权限过滤层 | `tools/`, `server-methods/` |
| 8 | FTS5 BM25、向量检索、混合排序、MMR | `context-engine/`, `memory/` |
| 9 | 扩展点验证（开闭原则）、`init()` 自注册 | `channels/plugins/` 多渠道 |

---

## 开始写代码前的建议

1. **先把 Phase 1 跑通**，哪怕代码很丑。跑通意味着：你发消息，AI 流式回复。
2. **不要提前抽象**。Phase 1 的代码直接用 Telegram 的具体类型，等到 Phase 5 再提取 interface。过早抽象会让你迷失在设计里而不是在学东西。
3. **每个 Phase 写测试**。尤其是 Session Store（测原子写入）、RPC Router（测方法分发）、Tool Loop（mock AI 返回 tool_use）。
4. **对照源码学习**。每实现一个模块，打开 `D:/A/code/claude/openclaw/src/` 对应目录，看 TS 实现和你的 Go 实现有何异同。
5. **遇到并发问题先用 `-race` 检测**：`go run -race main.go`，Go 的 race detector 非常准确。

---

*指南版本：2026-03-14*
*参考架构文档：`architecture.md`*
