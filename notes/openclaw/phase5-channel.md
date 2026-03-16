# Phase 5 — Channel 抽象：统一接口 + ChannelManager

> 前置：Phase 4 完成，配置热重载正常
> 目标：用 Go interface 隔离 Telegram 细节，引入 ChannelManager 统一管理渠道生命周期
> 对应 OpenClaw 模块：`src/channels/plugins/types.ts`、`src/gateway/server-channels.ts`

---

## 本阶段要建立的目录结构

```
goclaw/
└── internal/
    ├── ai/              ← 不变
    ├── session/         ← 不变
    ├── config/          ← 不变
    ├── gateway/         ← 修改：接受 ChannelManager
    ├── channel/         ← 新增（核心）
    │   ├── types.go     # Channel 接口、消息类型、能力接口
    │   ├── registry.go  # 渠道工厂注册表
    │   └── manager.go   # ChannelManager 生命周期管理
    └── channel/
        └── telegram/    ← 重构：从 internal/telegram/ 迁移
            └── telegram.go  # 实现 channel.Channel 接口
```

---

## 设计原则：小接口 + 能力检测

Go 的接口设计哲学：接口应该尽可能小，只表达必要能力。
OpenClaw 用 20+ 个 Adapter 组合，Go 用接口嵌套和类型断言来实现相同效果。

**反例（臃肿接口）：**
```go
// ❌ 一个巨大接口，实现负担重
type Channel interface {
    Send(...)
    Edit(...)
    Delete(...)
    SendTyping(...)
    SendPhoto(...)
    // ... 20+ 个方法
}
```

**本方案（小接口 + 能力检测）：**
```go
// ✅ 核心接口只有必须的方法
type Channel interface { ... } // 5 个方法

// ✅ 可选能力用独立接口，通过类型断言检测
type TypingIndicator interface { SendTyping(...) }
type MessageEditor   interface { Edit(...) }
```

---

## 第一步：核心类型定义

```go
// internal/channel/types.go

package channel

import (
    "context"
    "time"
)

// ── 标准化消息类型（与平台无关）────────────────────────

// InboundMessage 是从渠道收到的标准化消息
// 无论来自 Telegram、Discord 还是 Slack，统一用这个结构
type InboundMessage struct {
    // 元数据（用于构造 SessionKey）
    ChannelID string // "telegram" | "discord" | "slack"
    AccountID string // 渠道账号标识

    // 路由信息
    PeerID   string // 私聊时是用户 ID，群组时是群组 ID
    UserID   string // 发送者 ID
    ChatType string // "private" | "group" | "supergroup"

    // 内容
    Text        string
    ReplyToID   string       // 引用回复的消息 ID（可选）
    Attachments []Attachment // 附件（图片、文件等）

    // 时间
    Timestamp time.Time

    // 原始平台数据（调试用，各 Channel 实现自定义）
    Raw any
}

// OutboundMessage 是要发送到渠道的标准化消息
type OutboundMessage struct {
    PeerID    string
    Text      string
    ReplyToID string // 引用哪条消息回复（可选）
    ParseMode ParseMode
}

type ParseMode string

const (
    ParseModeNone     ParseMode = ""
    ParseModeMarkdown ParseMode = "markdown"
    ParseModeHTML     ParseMode = "html"
)

type Attachment struct {
    Type     string // "photo" | "document" | "audio"
    FileID   string
    FileName string
    MimeType string
    Size     int64
}

// ChannelStatus 描述渠道的当前状态
type ChannelStatus struct {
    Connected bool
    AccountID string
    Error     string    // 最近一次错误（连接时记录）
    Since     time.Time // 当前状态持续时间
}

// ── 入站消息处理函数类型 ───────────────────────────────

// InboundHandler 是 ChannelManager 注入给每个 Channel 的回调函数
// Channel 收到消息后调用此函数，由 ChannelManager 路由到正确的 Agent
type InboundHandler func(ctx context.Context, msg InboundMessage)

// ── 核心 Channel 接口 ─────────────────────────────────

// Channel 是每个渠道必须实现的核心接口
// 只包含最基础的能力，可选能力通过独立接口扩展
type Channel interface {
    // ID 返回渠道类型标识，如 "telegram"
    ID() string

    // AccountID 返回当前账号标识
    AccountID() string

    // Start 启动渠道（开始接收消息），阻塞直到 ctx 取消或发生错误
    Start(ctx context.Context) error

    // Stop 优雅关闭渠道
    Stop() error

    // Send 发送消息到指定 peer
    // 返回发出的消息 ID（用于后续 Edit/Delete）
    Send(ctx context.Context, msg OutboundMessage) (messageID string, err error)

    // Status 返回渠道当前状态
    Status() ChannelStatus
}

// ── 可选能力接口（通过类型断言检测是否支持）──────────────

// TypingIndicator 支持发送"正在输入"状态
type TypingIndicator interface {
    SendTyping(ctx context.Context, peerID string) error
}

// MessageEditor 支持编辑已发送的消息（流式输出需要）
type MessageEditor interface {
    Edit(ctx context.Context, peerID string, messageID string, text string) error
}

// MessageDeleter 支持删除消息
type MessageDeleter interface {
    Delete(ctx context.Context, peerID string, messageID string) error
}

// StreamSender 支持高效流式发送（自带节流，无需外部管理）
type StreamSender interface {
    SendStream(ctx context.Context, peerID string, textCh <-chan string) error
}
```

---

## 第二步：渠道注册表

```go
// internal/channel/registry.go

package channel

import (
    "fmt"
    "sync"
)

// Factory 是创建 Channel 实例的工厂函数
// cfg：从配置文件中该渠道对应的配置（map 形式，各渠道自行解析）
// handler：ChannelManager 注入的消息处理回调
type Factory func(accountID string, cfg map[string]any, handler InboundHandler) (Channel, error)

var (
    mu       sync.RWMutex
    registry = map[string]Factory{}
)

// Register 注册渠道工厂（在各渠道包的 init() 中调用）
func Register(channelID string, f Factory) {
    mu.Lock()
    defer mu.Unlock()
    registry[channelID] = f
}

// Create 根据渠道 ID 创建 Channel 实例
func Create(channelID, accountID string, cfg map[string]any, handler InboundHandler) (Channel, error) {
    mu.RLock()
    f, ok := registry[channelID]
    mu.RUnlock()

    if !ok {
        return nil, fmt.Errorf("unknown channel %q (did you import the channel package?)", channelID)
    }
    return f(accountID, cfg, handler)
}

// Registered 返回所有已注册的渠道 ID 列表
func Registered() []string {
    mu.RLock()
    defer mu.RUnlock()
    ids := make([]string, 0, len(registry))
    for id := range registry {
        ids = append(ids, id)
    }
    return ids
}
```

---

## 第三步：ChannelManager

```go
// internal/channel/manager.go

package channel

import (
    "context"
    "log"
    "sync"
    "time"
)

// entry 记录一个活跃渠道的运行信息
type entry struct {
    ch     Channel
    cancel context.CancelFunc
    done   chan struct{} // 渠道 Start() 返回后关闭
}

// Manager 统一管理所有渠道的生命周期
// 职责：启动、停止、重启、健康监测
type Manager struct {
    handler InboundHandler // 所有渠道共用同一个入站处理函数

    mu      sync.RWMutex
    entries map[string]*entry // key = channelID + ":" + accountID
}

func NewManager(handler InboundHandler) *Manager {
    return &Manager{
        handler: handler,
        entries: make(map[string]*entry),
    }
}

// Start 启动一个渠道（非阻塞，在后台 goroutine 运行）
func (m *Manager) Start(parentCtx context.Context, channelID, accountID string, cfg map[string]any) error {
    key := channelID + ":" + accountID

    m.mu.Lock()
    if _, exists := m.entries[key]; exists {
        m.mu.Unlock()
        return fmt.Errorf("channel %s already running", key)
    }
    m.mu.Unlock()

    // 注入 handler：Channel 收到消息后调用 m.handler
    ch, err := Create(channelID, accountID, cfg, m.handler)
    if err != nil {
        return fmt.Errorf("create channel %s: %w", key, err)
    }

    ctx, cancel := context.WithCancel(parentCtx)
    e := &entry{
        ch:     ch,
        cancel: cancel,
        done:   make(chan struct{}),
    }

    m.mu.Lock()
    m.entries[key] = e
    m.mu.Unlock()

    // 在后台 goroutine 运行 Channel.Start()
    // 带自动重连：如果 Start 返回错误（非 ctx 取消），等待后重试
    go m.runWithReconnect(ctx, key, e)

    return nil
}

// runWithReconnect 带指数退避的重连循环
func (m *Manager) runWithReconnect(ctx context.Context, key string, e *entry) {
    defer close(e.done)

    backoff := 1 * time.Second
    maxBackoff := 60 * time.Second

    for {
        log.Printf("[channel] starting %s...", key)
        err := e.ch.Start(ctx)

        // ctx 取消（正常关闭）
        if ctx.Err() != nil {
            log.Printf("[channel] %s stopped (context cancelled)", key)
            return
        }

        // 发生错误，等待后重试
        if err != nil {
            log.Printf("[channel] %s error: %v, retrying in %s...", key, err, backoff)
        }

        select {
        case <-time.After(backoff):
            backoff = min(backoff*2, maxBackoff) // 指数退避
        case <-ctx.Done():
            return
        }
    }
}

// Stop 停止指定渠道
func (m *Manager) Stop(channelID, accountID string) error {
    key := channelID + ":" + accountID

    m.mu.Lock()
    e, ok := m.entries[key]
    if ok {
        delete(m.entries, key)
    }
    m.mu.Unlock()

    if !ok {
        return fmt.Errorf("channel %s not running", key)
    }

    e.cancel() // 触发 ctx 取消
    e.ch.Stop()
    <-e.done   // 等待 Start goroutine 退出

    return nil
}

// Restart 重启渠道（Stop + Start）
func (m *Manager) Restart(parentCtx context.Context, channelID, accountID string, cfg map[string]any) error {
    if err := m.Stop(channelID, accountID); err != nil {
        log.Printf("[channel] stop before restart failed: %v (ignoring)", err)
    }
    return m.Start(parentCtx, channelID, accountID, cfg)
}

// StopAll 停止所有渠道（优雅关闭时使用）
func (m *Manager) StopAll() {
    m.mu.Lock()
    entries := make(map[string]*entry, len(m.entries))
    for k, v := range m.entries {
        entries[k] = v
    }
    m.entries = make(map[string]*entry)
    m.mu.Unlock()

    var wg sync.WaitGroup
    for key, e := range entries {
        wg.Add(1)
        go func(key string, e *entry) {
            defer wg.Done()
            log.Printf("[channel] stopping %s...", key)
            e.cancel()
            e.ch.Stop()
            <-e.done
        }(key, e)
    }
    wg.Wait()
}

// Status 返回所有渠道的状态快照
func (m *Manager) Status() map[string]ChannelStatus {
    m.mu.RLock()
    defer m.mu.RUnlock()
    result := make(map[string]ChannelStatus, len(m.entries))
    for key, e := range m.entries {
        result[key] = e.ch.Status()
    }
    return result
}

func min(a, b time.Duration) time.Duration {
    if a < b {
        return a
    }
    return b
}
```

---

## 第四步：重构 Telegram 实现

将 Phase 1-4 的 `internal/telegram/` 包重构，实现 `channel.Channel` 接口。

```go
// internal/channel/telegram/telegram.go

package telegram

import (
    "context"
    "fmt"
    "log"
    "net/http"
    "time"

    "github.com/yourname/goclaw/internal/channel"
)

// 编译期接口检查：确保 *Bot 实现了所有需要的接口
// 如果缺少方法，编译器会报错，而不是运行时 panic
var (
    _ channel.Channel        = (*Bot)(nil)
    _ channel.TypingIndicator = (*Bot)(nil)
    _ channel.MessageEditor  = (*Bot)(nil)
)

type Config struct {
    Token string `yaml:"token"`
}

type Bot struct {
    accountID string
    token     string
    apiBase   string
    client    *http.Client
    handler   channel.InboundHandler

    status channel.ChannelStatus
}

func New(accountID string, cfg map[string]any, handler channel.InboundHandler) (*Bot, error) {
    token, _ := cfg["token"].(string)
    if token == "" {
        return nil, fmt.Errorf("telegram: token is required")
    }
    return &Bot{
        accountID: accountID,
        token:     token,
        apiBase:   "https://api.telegram.org/bot" + token,
        client:    &http.Client{Timeout: 35 * time.Second},
        handler:   handler,
        status: channel.ChannelStatus{
            AccountID: accountID,
            Since:     time.Now(),
        },
    }, nil
}

// ── 实现 channel.Channel 接口 ──────────────────────────

func (b *Bot) ID() string        { return "telegram" }
func (b *Bot) AccountID() string { return b.accountID }

func (b *Bot) Start(ctx context.Context) error {
    log.Printf("[telegram:%s] starting...", b.accountID)
    b.status.Connected = true
    b.status.Since = time.Now()

    err := b.poll(ctx)

    b.status.Connected = false
    if err != nil {
        b.status.Error = err.Error()
    }
    return err
}

func (b *Bot) Stop() error {
    // Long Polling 通过 ctx 取消停止，这里无需额外操作
    return nil
}

func (b *Bot) Send(ctx context.Context, msg channel.OutboundMessage) (string, error) {
    result, err := b.sendMessage(msg.PeerID, msg.Text, msg.ParseMode)
    if err != nil {
        return "", err
    }
    return fmt.Sprintf("%d", result.MessageID), nil
}

func (b *Bot) Status() channel.ChannelStatus {
    return b.status
}

// ── 实现可选能力接口 ─────────────────────────────────

func (b *Bot) SendTyping(ctx context.Context, peerID string) error {
    return b.apiCall("sendChatAction", map[string]string{
        "chat_id": peerID,
        "action":  "typing",
    }, nil)
}

func (b *Bot) Edit(ctx context.Context, peerID, messageID, text string) error {
    return b.apiCall("editMessageText", map[string]string{
        "chat_id":    peerID,
        "message_id": messageID,
        "text":       text,
        "parse_mode": "Markdown",
    }, nil)
}

// ── 轮询循环（内部方法）────────────────────────────────

func (b *Bot) poll(ctx context.Context) error {
    offset := 0
    for {
        select {
        case <-ctx.Done():
            return nil
        default:
        }

        updates, err := b.getUpdates(ctx, offset, 30)
        if err != nil {
            return err
        }

        for _, u := range updates {
            offset = u.UpdateID + 1
            if u.Message != nil && u.Message.Text != "" {
                msg := b.convertMessage(u.Message)
                go b.handler(ctx, msg)
            }
        }
    }
}

// convertMessage 将 Telegram 原生消息转换为标准 InboundMessage
func (b *Bot) convertMessage(m *tgMessage) channel.InboundMessage {
    chatType := "private"
    peerID := fmt.Sprintf("%d", m.From.ID)
    if m.Chat.Type != "private" {
        chatType = m.Chat.Type
        peerID = fmt.Sprintf("%d", m.Chat.ID)
    }
    return channel.InboundMessage{
        ChannelID: "telegram",
        AccountID: b.accountID,
        PeerID:    peerID,
        UserID:    fmt.Sprintf("%d", m.From.ID),
        ChatType:  chatType,
        Text:      m.Text,
        Timestamp: time.Now(),
        Raw:       m,
    }
}

// init 注册 Telegram 工厂函数
// 导入此包时自动注册，无需手动调用
func init() {
    channel.Register("telegram", func(accountID string, cfg map[string]any, handler channel.InboundHandler) (channel.Channel, error) {
        return New(accountID, cfg, handler)
    })
}
```

---

## 第五步：Gateway 集成 ChannelManager

Gateway 的 `chat.send` 方法需要能通过 ChannelManager 发送回复。
现在回复路径是：`Gateway → ChannelManager → 对应 Channel → 平台`。

```go
// internal/gateway/methods/chat.go（关键变更）

// 发送回复时，不再直接引用 Telegram，而是通过 ChannelManager
func (h *ChatHandler) sendReply(ctx context.Context, sess *session.Session, text string) error {
    key := sess.Key

    outMsg := channel.OutboundMessage{
        PeerID:    key.PeerID,
        Text:      text,
        ParseMode: channel.ParseModeMarkdown,
    }

    ch, err := h.channelMgr.Get(key.ChannelID, key.AccountID)
    if err != nil {
        return fmt.Errorf("get channel: %w", err)
    }

    // 检测是否支持流式发送（StreamSender 接口）
    if ss, ok := ch.(channel.StreamSender); ok {
        // 使用渠道内置的流式发送（自带节流）
        return ss.SendStream(ctx, key.PeerID, h.textCh)
    }

    // 降级：先发占位消息，流结束后一次性更新
    _, err = ch.Send(ctx, outMsg)
    return err
}
```

### 为 Telegram 实现 StreamSender

```go
// internal/channel/telegram/stream.go

// SendStream 实现 channel.StreamSender，包含节流逻辑
// 这样节流代码和 Telegram 耦合在一起，而不是散落在 Gateway 里
func (b *Bot) SendStream(ctx context.Context, peerID string, textCh <-chan string) error {
    // 发送占位消息
    result, err := b.sendMessage(peerID, "…", channel.ParseModeNone)
    if err != nil {
        return err
    }
    msgID := fmt.Sprintf("%d", result.MessageID)

    var buf strings.Builder
    ticker := time.NewTicker(300 * time.Millisecond)
    defer ticker.Stop()
    lastSent := ""

    flush := func() {
        current := buf.String()
        if current == lastSent || current == "" {
            return
        }
        b.Edit(ctx, peerID, msgID, current)
        lastSent = current
    }

    for {
        select {
        case chunk, ok := <-textCh:
            if !ok {
                flush()
                return nil
            }
            buf.WriteString(chunk)
        case <-ticker.C:
            flush()
        case <-ctx.Done():
            flush()
            return nil
        }
    }
}

// 确保实现了 StreamSender 接口
var _ channel.StreamSender = (*Bot)(nil)
```

---

## 第六步：修改 main.go

```go
// main.go（关键部分）

import (
    _ "github.com/yourname/goclaw/internal/channel/telegram" // 触发 init() 注册
)

func main() {
    cfgMgr, _ := config.NewManager("config.yaml")
    cfg := cfgMgr.Get()

    store, _ := session.NewFileStore(cfg.Session.Dir)
    aiClient := anthropic.New(cfg.AI.APIKey, cfg.AI.Model, cfg.AI.SystemPrompt)

    // 创建入站处理函数（消息路由到 Gateway）
    gw := gateway.New(cfgMgr, aiClient, store)
    inboundHandler := gw.InboundHandler() // Gateway 暴露的统一入站处理函数

    // 创建 ChannelManager
    chanMgr := channel.NewManager(inboundHandler)

    ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer cancel()

    // 启动 Telegram 渠道
    chanMgr.Start(ctx, "telegram", cfg.Telegram.AccountID, map[string]any{
        "token": cfg.Telegram.Token,
    })

    // 配置变更时重启受影响的渠道
    cfgMgr.OnChange(func(old, new *config.Config) {
        if old.Telegram.Token != new.Telegram.Token {
            chanMgr.Restart(ctx, "telegram", new.Telegram.AccountID, map[string]any{
                "token": new.Telegram.Token,
            })
        }
    })

    defer chanMgr.StopAll()

    gw.Start(ctx) // 阻塞
}
```

---

## 本阶段核心工程知识点

### 1. 编译期接口检查

```go
// 在实现文件顶部写这几行，编译期立即发现接口缺失
var (
    _ channel.Channel         = (*Bot)(nil)
    _ channel.TypingIndicator = (*Bot)(nil)
    _ channel.MessageEditor   = (*Bot)(nil)
)
// 如果 Bot 缺少某个方法，编译器报错：
// cannot use (*Bot)(nil) (type *Bot) as type channel.Channel:
//   *Bot does not implement channel.Channel (missing Stop method)
```

### 2. `init()` 自注册模式

```go
// internal/channel/telegram/telegram.go
func init() {
    channel.Register("telegram", New)
}

// main.go
import _ "github.com/yourname/goclaw/internal/channel/telegram"
// 只要导入（即使不用任何导出符号），init() 就会执行，自动注册
```

这是 Go 的标准插件化模式，数据库驱动（`database/sql`）、图像格式等都用此模式。

### 3. 能力检测（Capability Detection）替代巨大接口

```go
// 发送"正在输入"时的正确写法
func sendTypingIfSupported(ctx context.Context, ch channel.Channel, peerID string) {
    if ti, ok := ch.(channel.TypingIndicator); ok {
        ti.SendTyping(ctx, peerID)
    }
    // 不支持就什么都不做，不报错
}
```

这比在 `Channel` 接口里加 `SendTyping()` 更好：不是所有渠道都支持，
SMS 渠道就没有"正在输入"这个概念。

### 4. 指数退避重连

```go
backoff := 1 * time.Second
maxBackoff := 60 * time.Second

for {
    err := e.ch.Start(ctx)
    if ctx.Err() != nil { return } // 正常关闭，不重连

    log.Printf("channel error: %v, retry in %s", err, backoff)
    time.Sleep(backoff)
    backoff = min(backoff*2, maxBackoff) // 1s → 2s → 4s → ... → 60s
}
```

---

## 与 OpenClaw 的对应关系

| 本阶段代码 | OpenClaw 对应 |
|-----------|---------------|
| `channel.Channel` 接口 | `ChannelPlugin` 类型（`src/channels/plugins/types.ts`） |
| `TypingIndicator`、`MessageEditor` | `ChannelTypingAdapter`、`ChannelStreamingAdapter` 等 |
| `channel.Register` / `init()` | OpenClaw 的 plugin 注册机制 |
| `ChannelManager.runWithReconnect` | `server-channels.ts` 的退避重试逻辑 |
| `ChannelManager.StopAll` | `server-channels.ts` 的优雅关闭 |
| `convertMessage` | OpenClaw 各渠道的 `parseInboundMessage` |

---

## 验证：开闭原则

Phase 5 完成后，你可以做一个验证：
> 复制 `internal/channel/telegram/` 目录，创建 `internal/channel/mock/`，
> 实现一个打印到终端的 Mock Channel。只改 `main.go` 的 import 和 `chanMgr.Start` 调用，
> Gateway、Session、AI 代码**一行都不改**就能切换渠道。
> 如果需要改动其他文件，说明抽象还不够干净。

---

## 下一阶段预告

Phase 5 的每次对话都使用同一个 AI 客户端和固定配置。
Phase 6 将引入 **Agent 抽象**：多个独立 Agent，各自有独立配置、
独立工作区，模型故障时自动 fallback，并支持用 run_id 中止正在进行的推理。
