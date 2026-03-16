# Phase 9 — 多渠道扩展：接入 Discord，验证架构

> 前置：Phase 8 完成，记忆系统正常
> 目标：接入 Discord，验证 Channel 抽象层的开闭原则；完善消息路由（Bindings）
> 对应 OpenClaw 模块：`src/channels/discord/`、`src/routing/resolve-route.ts`、`src/gateway/server-channels.ts`

---

## 本阶段目录变动

```
goclaw/
└── internal/
    ├── channel/
    │   ├── types.go         ← 不变
    │   ├── registry.go      ← 不变
    │   ├── manager.go       ← 不变
    │   ├── telegram/        ← 不变（！）
    │   └── discord/         ← 新增（仅此处有改动）
    │       └── discord.go   # 实现 channel.Channel 接口
    ├── routing/             ← 新增
    │   └── resolver.go      # resolveAgentRoute()：Bindings 路由
    └── gateway/
        └── methods/
            └── channels.go  ← 新增：channels.status RPC 方法
```

**验收标准：**
> Gateway、Agent、Session、Memory、Tools 任何一个文件都不应该修改。
> 如果你需要改动这些文件才能接入 Discord，说明 Phase 5 的 Channel 抽象有遗漏。

---

## 第一步：实现 Discord Channel

```go
// internal/channel/discord/discord.go

package discord

import (
    "context"
    "fmt"
    "log"
    "strings"
    "sync"
    "time"

    "github.com/bwmarrin/discordgo"
    "github.com/yourname/goclaw/internal/channel"
)

// 编译期接口检查
var (
    _ channel.Channel        = (*Discord)(nil)
    _ channel.TypingIndicator = (*Discord)(nil)
    _ channel.MessageEditor  = (*Discord)(nil)
    _ channel.StreamSender   = (*Discord)(nil)
)

// Discord 实现 channel.Channel 接口
type Discord struct {
    accountID string
    session   *discordgo.Session
    handler   channel.InboundHandler
    botUserID string // 自己的 User ID，用于过滤自己发的消息

    mu     sync.RWMutex
    status channel.ChannelStatus
}

func New(accountID string, cfg map[string]any, handler channel.InboundHandler) (*Discord, error) {
    token, _ := cfg["token"].(string)
    if token == "" {
        return nil, fmt.Errorf("discord: token is required")
    }

    // Discord Bot Token 必须加 "Bot " 前缀
    dg, err := discordgo.New("Bot " + token)
    if err != nil {
        return nil, fmt.Errorf("discord: create session: %w", err)
    }

    // 申请需要的 Intent（必须在 Discord 开发者面板开启）
    dg.Identify.Intents = discordgo.IntentsGuildMessages |
        discordgo.IntentsDirectMessages |
        discordgo.IntentsMessageContent // 需要 privileged intent

    d := &Discord{
        accountID: accountID,
        session:   dg,
        handler:   handler,
        status: channel.ChannelStatus{
            AccountID: accountID,
            Since:     time.Now(),
        },
    }

    // 注册消息处理函数
    dg.AddHandler(d.onMessage)

    return d, nil
}

// ── 实现 channel.Channel 接口 ──────────────────────────

func (d *Discord) ID() string        { return "discord" }
func (d *Discord) AccountID() string { return d.accountID }

func (d *Discord) Start(ctx context.Context) error {
    log.Printf("[discord:%s] connecting...", d.accountID)

    // 连接 Discord WebSocket 网关
    if err := d.session.Open(); err != nil {
        return fmt.Errorf("discord: open session: %w", err)
    }

    // 保存自己的 User ID（用于过滤自发消息）
    d.botUserID = d.session.State.User.ID

    d.mu.Lock()
    d.status.Connected = true
    d.status.Error = ""
    d.status.Since = time.Now()
    d.mu.Unlock()

    log.Printf("[discord:%s] connected as %s", d.accountID, d.session.State.User.Username)

    // 阻塞直到 ctx 取消
    <-ctx.Done()
    return nil
}

func (d *Discord) Stop() error {
    log.Printf("[discord:%s] stopping...", d.accountID)
    err := d.session.Close()
    d.mu.Lock()
    d.status.Connected = false
    d.status.Since = time.Now()
    d.mu.Unlock()
    return err
}

func (d *Discord) Send(ctx context.Context, msg channel.OutboundMessage) (string, error) {
    text := msg.Text
    // Discord 使用 Markdown，但语法略有不同
    // Telegram 的 *bold* 在 Discord 也是 **bold**，基本兼容

    m, err := d.session.ChannelMessageSend(msg.PeerID, text)
    if err != nil {
        return "", fmt.Errorf("discord send: %w", err)
    }
    return m.ID, nil
}

func (d *Discord) Status() channel.ChannelStatus {
    d.mu.RLock()
    defer d.mu.RUnlock()
    return d.status
}

// ── 实现可选能力接口 ─────────────────────────────────

func (d *Discord) SendTyping(ctx context.Context, channelID string) error {
    return d.session.ChannelTyping(channelID)
}

func (d *Discord) Edit(ctx context.Context, channelID, messageID, text string) error {
    _, err := d.session.ChannelMessageEdit(channelID, messageID, text)
    return err
}

// SendStream 流式更新 Discord 消息（与 Telegram 实现相同的节流逻辑）
func (d *Discord) SendStream(ctx context.Context, channelID string, textCh <-chan string) error {
    // 先发送占位消息
    m, err := d.session.ChannelMessageSend(channelID, "…")
    if err != nil {
        return err
    }

    var buf strings.Builder
    ticker := time.NewTicker(500 * time.Millisecond) // Discord 频率限制更严，用 500ms
    defer ticker.Stop()
    lastSent := ""

    flush := func() {
        current := buf.String()
        if current == lastSent || current == "" {
            return
        }
        d.session.ChannelMessageEdit(channelID, m.ID, current)
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

// ── 消息处理（内部）─────────────────────────────────

func (d *Discord) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
    // 忽略自己发的消息（防止 Bot 自己回复自己，无限循环）
    if m.Author.ID == d.botUserID {
        return
    }
    // 忽略空消息
    if strings.TrimSpace(m.Content) == "" {
        return
    }

    // 确定会话范围
    // Discord 没有"私聊"和"群聊"的概念，用 channel 类型区分
    // DM channel → ScopeDM，Guild channel → ScopeGroup
    chatType := "group"
    peerID := m.ChannelID // Discord 用 channel ID 路由消息

    // 检查是否是 DM
    ch, err := s.State.Channel(m.ChannelID)
    if err == nil && ch.Type == discordgo.ChannelTypeDM {
        chatType = "private"
    }

    inbound := channel.InboundMessage{
        ChannelID: "discord",
        AccountID: d.accountID,
        PeerID:    peerID,
        UserID:    m.Author.ID,
        ChatType:  chatType,
        Text:      m.Content,
        Timestamp: m.Timestamp,
        Raw:       m,
    }

    go d.handler(context.Background(), inbound)
}

// init 自注册（导入包时自动注册，无需手动调用）
func init() {
    channel.Register("discord", func(accountID string, cfg map[string]any, handler channel.InboundHandler) (channel.Channel, error) {
        return New(accountID, cfg, handler)
    })
}
```

---

## 第二步：消息路由（Bindings）

现在有两个渠道，消息路由需要根据 `bindings` 配置决定哪个渠道的消息路由到哪个 Agent。

```go
// internal/routing/resolver.go

package routing

import (
    "github.com/yourname/goclaw/internal/channel"
    "github.com/yourname/goclaw/internal/config"
    "github.com/yourname/goclaw/internal/session"
)

// Route 描述一条消息的路由结果
type Route struct {
    AgentID    string
    SessionKey session.SessionKey
}

// Resolver 根据配置的 Bindings 解析消息路由
// 对应 OpenClaw 的 resolveAgentRoute() 函数
type Resolver struct {
    cfgMgr *config.Manager
}

func NewResolver(cfgMgr *config.Manager) *Resolver {
    return &Resolver{cfgMgr: cfgMgr}
}

// Resolve 为一条入站消息解析路由
// 返回目标 AgentID 和 SessionKey
func (r *Resolver) Resolve(msg channel.InboundMessage) Route {
    cfg := r.cfgMgr.Get()

    // 按 Bindings 列表顺序匹配，第一个匹配的规则生效
    agentID := r.matchBinding(cfg.Bindings, msg)

    // 根据消息类型决定会话隔离级别
    scope := session.ScopeDM
    if msg.ChatType != "private" {
        scope = session.ScopeGroup
    }

    key := session.SessionKey{
        ChannelID: msg.ChannelID,
        AccountID: msg.AccountID,
        Scope:     scope,
        PeerID:    msg.PeerID,
        AgentID:   agentID,
    }

    return Route{
        AgentID:    agentID,
        SessionKey: key,
    }
}

// matchBinding 返回第一个匹配的 Binding 的 AgentID
// 没有匹配时返回 "default"
func (r *Resolver) matchBinding(bindings []config.BindingConfig, msg channel.InboundMessage) string {
    for _, b := range bindings {
        if b.Match.Channel != "" && b.Match.Channel != msg.ChannelID {
            continue
        }
        if b.Match.AccountID != "" && b.Match.AccountID != msg.AccountID {
            continue
        }
        return b.AgentID
    }
    return "default"
}
```

---

## 第三步：Gateway 集成 Resolver

入站消息从 ChannelManager 到 Gateway 的路径：

```go
// internal/gateway/server.go（修改 InboundHandler）

func (g *Gateway) InboundHandler() channel.InboundHandler {
    return func(ctx context.Context, msg channel.InboundMessage) {
        // 1. 解析路由（哪个 Agent 处理这条消息）
        route := g.resolver.Resolve(msg)

        // 2. 获取 Agent
        ag, err := g.agentRegistry.Get(route.AgentID)
        if err != nil {
            log.Printf("[gateway] agent not found: %s", route.AgentID)
            return
        }

        // 3. 加载会话
        sess, err := g.store.Get(route.SessionKey)
        if err != nil {
            log.Printf("[gateway] get session error: %v", err)
            return
        }

        // 4. 生成 run_id
        runID := fmt.Sprintf("run-%d", time.Now().UnixNano())

        // 5. 创建事件 channel（流式输出和状态事件）
        eventCh := make(chan agent.AgentEvent, 64)
        go g.handleAgentEvents(ctx, msg, eventCh)

        // 6. 执行 Agent
        result, err := ag.RunReply(ctx, sess, msg.Text, runID, eventCh)
        close(eventCh)

        if err != nil {
            if !errors.Is(err, agent.ErrAborted) {
                log.Printf("[gateway] agent error: %v", err)
            }
            return
        }

        // 7. 通过 ChannelManager 发送回复
        ch, err := g.channelMgr.Get(msg.ChannelID, msg.AccountID)
        if err != nil {
            return
        }

        if ss, ok := ch.(channel.StreamSender); ok {
            // 如果支持流式发送，把流式输出传给渠道
            // （实际上这里已经是结果了，流式输出在 handleAgentEvents 里处理）
            _ = ss
        }

        ch.Send(ctx, channel.OutboundMessage{
            PeerID:    msg.PeerID,
            Text:      result.Reply,
            ParseMode: channel.ParseModeMarkdown,
        })
    }
}

func (g *Gateway) handleAgentEvents(ctx context.Context, msg channel.InboundMessage, eventCh <-chan agent.AgentEvent) {
    ch, err := g.channelMgr.Get(msg.ChannelID, msg.AccountID)
    if err != nil {
        return
    }

    // 流式发送：实时把 chat.delta 推给渠道
    if ss, ok := ch.(channel.StreamSender); ok {
        textCh := make(chan string, 32)
        go func() {
            defer close(textCh)
            for e := range eventCh {
                if e.Type == "chat.delta" {
                    if data, ok := e.Data.(map[string]string); ok {
                        textCh <- data["chunk"]
                    }
                }
                // 同时广播给 WebSocket 客户端
                g.hub.Broadcast(gateway.NewEvent(e.Type, e.Data))
            }
        }()
        ss.SendStream(ctx, msg.PeerID, textCh)
    } else {
        // 不支持流式：只广播事件，最终一次性发送
        for e := range eventCh {
            g.hub.Broadcast(gateway.NewEvent(e.Type, e.Data))
        }
    }
}
```

---

## 第四步：channels.status RPC 方法

```go
// internal/gateway/methods/channels.go

package methods

import (
    "context"
    "encoding/json"

    "github.com/yourname/goclaw/internal/channel"
)

type ChannelsHandler struct {
    manager *channel.Manager
}

func NewChannelsHandler(manager *channel.Manager) *ChannelsHandler {
    return &ChannelsHandler{manager: manager}
}

// channels.status：返回所有渠道的状态
func (h *ChannelsHandler) Status(ctx context.Context, _ json.RawMessage) (any, error) {
    return h.manager.Status(), nil
}

// channels.list：返回所有已注册渠道类型
func (h *ChannelsHandler) List(ctx context.Context, _ json.RawMessage) (any, error) {
    return channel.Registered(), nil
}
```

---

## 第五步：更新配置和 main.go

```yaml
# config.yaml（新增 Discord 配置）

telegram:
  token: "${TELEGRAM_BOT_TOKEN}"
  account_id: "tg-main"

discord:
  token: "${DISCORD_BOT_TOKEN}"
  account_id: "dc-main"

# 路由规则：不同渠道路由到不同 Agent
bindings:
  - agent_id: default
    match:
      channel: telegram     # 所有 Telegram 消息 → default agent

  - agent_id: default
    match:
      channel: discord      # 所有 Discord 消息 → default agent

# 如果你想用不同 Agent 处理不同渠道：
# bindings:
#   - agent_id: telegram-agent
#     match:
#       channel: telegram
#   - agent_id: discord-agent
#     match:
#       channel: discord
```

```go
// main.go（关键变更：导入 discord 包，启动 Discord 渠道）

import (
    _ "github.com/yourname/goclaw/internal/channel/telegram" // 注册 telegram
    _ "github.com/yourname/goclaw/internal/channel/discord"  // 注册 discord（新增）
)

func main() {
    // ... 初始化代码不变 ...

    ctx, cancel := signal.NotifyContext(...)
    defer cancel()

    // 启动 Telegram
    chanMgr.Start(ctx, "telegram", cfg.Telegram.AccountID, map[string]any{
        "token": cfg.Telegram.Token,
    })

    // 启动 Discord（新增这三行）
    chanMgr.Start(ctx, "discord", cfg.Discord.AccountID, map[string]any{
        "token": cfg.Discord.Token,
    })

    // Gateway、Session、Memory、Agent 代码一行都没改
    gw.Start(ctx)
}
```

---

## 验收：开闭原则测试

接入 Discord 后，统计各模块的改动量：

| 模块 | 改动 | 是否符合预期 |
|------|------|-------------|
| `internal/channel/discord/` | **新建** | ✅ 扩展点 |
| `main.go` | 导入 discord 包，增加 `chanMgr.Start` | ✅ 胶水代码 |
| `config.yaml` | 增加 `discord.token` 和 `bindings` | ✅ 配置变更 |
| `internal/gateway/` | 不变 | ✅ |
| `internal/agent/` | 不变 | ✅ |
| `internal/session/` | 不变 | ✅ |
| `internal/memory/` | 不变 | ✅ |
| `internal/tools/` | 不变 | ✅ |
| `internal/channel/telegram/` | 不变 | ✅ |

**如果你发现某个"不变"的模块被迫修改了**，找到原因并重构，使其满足开闭原则，然后再继续。

---

## 扩展：接入第三个渠道（Slack）

有了 Discord 的经验，接入 Slack 的步骤完全相同：

```
1. 创建 internal/channel/slack/slack.go
2. 实现 channel.Channel 接口（及可选能力接口）
3. 在 init() 中调用 channel.Register("slack", New)
4. main.go 中 import _ "...slack"，并 chanMgr.Start(ctx, "slack", ...)
5. config.yaml 中添加 slack token 和 bindings 规则
```

Slack 的特殊点（与 Telegram/Discord 的区别）：
- 使用 Bolt 框架，有独立的事件 API（需要 HTTP webhook 或 Socket Mode）
- 消息格式是 Block Kit，需要额外的格式转换
- 线程（Thread）是一等公民，对应 `SessionKey.ThreadID`

---

## 多渠道路由的高级场景

```yaml
# 场景 1：不同账号路由到不同 Agent
bindings:
  - agent_id: work-agent
    match:
      channel: slack
      account_id: "work-workspace"

  - agent_id: personal-agent
    match:
      channel: telegram
      account_id: "personal-bot"

# 场景 2：同一渠道的不同群组路由到不同 Agent
# （这需要在 BindingMatch 中加入 peerID 匹配，Phase 9 可扩展）
```

---

## 本阶段核心工程知识点

### 1. 开闭原则（OCP）验证

> 对扩展开放，对修改封闭。

Phase 5 设计的 Channel 接口是"扩展点"：新渠道只需在这个点扩展，不需要修改现有代码。
Phase 9 用 Discord 验证了这一点。

**衡量标准：**
- 好的抽象：接入新渠道改动 < 200 行，且只在 channel/ 子目录
- 需要重构：接入新渠道需要改动 gateway/、agent/ 等核心模块

### 2. `init()` 自注册的权衡

```go
// 优点：导入即注册，使用者无需知道实现细节
import _ "github.com/yourname/goclaw/internal/channel/discord"

// 缺点：如果多个包都在 init() 里做副作用，初始化顺序可能难以追踪
// 解决：init() 只做注册（幂等操作），不做 IO、不建立连接
```

这是 Go 标准库的惯用法（`database/sql`、`image` 包等都用此模式）。

### 3. Discord Gateway Intent 问题

Discord 的 `GUILD_MESSAGES` + `MESSAGE_CONTENT` 是 Privileged Intent，
需要在 Discord Developer Portal → Bot → Privileged Gateway Intents 手动开启，
否则 Bot 收不到消息内容（只能看到空字符串）。

这是一个常见陷阱，记录在此：
```go
dg.Identify.Intents = discordgo.IntentsGuildMessages |
    discordgo.IntentsDirectMessages |
    discordgo.IntentsMessageContent // ← 必须在 Discord 开发者面板开启 Privileged Intent
```

### 4. 渠道差异的处理策略

不同渠道的消息格式差异很大：

| 特性 | Telegram | Discord | Slack |
|------|----------|---------|-------|
| Markdown | 自定义 | CommonMark | mrkdwn |
| 最大长度 | 4096 字符 | 2000 字符 | 40000 字符 |
| 编辑消息 | 支持 | 支持 | 支持 |
| Typing 状态 | 5秒有效 | 即时 | 即时 |
| 线程 | 不支持 | 支持 | 支持 |

处理策略：
- 短期：在各 Channel 实现里做格式转换
- 长期：在 `OutboundMessage` 中加 `RichContent` 字段，各渠道按需渲染

---

## 与 OpenClaw 的对应关系

| 本阶段代码 | OpenClaw 对应 |
|-----------|---------------|
| `discord.go` + `init()` 注册 | `src/channels/discord/` + 渠道注册 |
| `routing.Resolver.Resolve` | `src/routing/resolve-route.ts` 的 `resolveAgentRoute()` |
| `routing.matchBinding` | `src/routing/bindings.ts` 的规则匹配 |
| `channels.status` RPC | `src/gateway/server-methods/channels.ts` |
| `SessionKey` 中的 `ChannelID`+`AccountID` | OpenClaw SessionKey 的相同字段 |

---

## 全项目回顾：你构建了什么

经过 9 个 Phase，你从零构建了一个完整的多渠道 AI 网关：

```
Phase 1  Telegram + 流式 AI 回复
Phase 2  多用户会话，文件持久化
Phase 3  WebSocket RPC Gateway，实时事件推送
Phase 4  配置热重载，atomic.Pointer，fsnotify
Phase 5  Channel 抽象，ChannelManager，init() 注册
Phase 6  Agent 执行链，多模型 Fallback，AbortRegistry
Phase 7  工具调用循环，JSON Schema，并发工具执行
Phase 8  SQLite + FTS5 记忆系统，BM25 + 时间衰减
Phase 9  接入 Discord，验证开闭原则，Bindings 路由
```

**对照 OpenClaw 完整架构：**

| OpenClaw 子系统 | 你的实现 |
|----------------|---------|
| `src/gateway/` | `internal/gateway/` |
| `src/channels/` | `internal/channel/` |
| `src/sessions/` | `internal/session/` |
| `src/agents/` | `internal/agent/` |
| `src/tools/` | `internal/tools/` |
| `src/context-engine/` + `src/memory/` | `internal/memory/` |
| `src/config/` | `internal/config/` |
| `src/routing/` | `internal/routing/` |

---

## 后续可以继续扩展的方向

1. **Web Dashboard**：用 Go 的 `embed` 嵌入静态前端，提供可视化管理界面
2. **CLI 完善**：用 `cobra` 补全 `goclaw gateway start/stop/status/doctor` 等命令
3. **向量检索**：引入 `sqlite-vec` 或 `pgvector`，实现语义搜索增强记忆检索
4. **代码执行沙箱**：用 `os/exec` + Docker API 实现隔离的代码执行工具
5. **Webhook 支持**：替换 Long Polling，用 HTTPS Webhook 接收 Telegram/Discord 消息
6. **TLS/HTTPS**：用 `crypto/tls` 或 `autocert` 为 Gateway 加 HTTPS
7. **Prometheus 指标**：暴露 `/metrics` 端点，监控 Agent 延迟、渠道健康等
8. **接入更多渠道**：Slack、WhatsApp、Signal（每个只需实现 channel.Channel 接口）
