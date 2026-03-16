# Phase 6 — Agent 抽象：多模型 Fallback + 中止机制

> 前置：Phase 5 完成，Channel 接口抽象正常
> 目标：独立 Agent 单元、模型故障自动切换、run_id 中止正在进行的推理
> 对应 OpenClaw 模块：`src/agents/`、`runReplyAgent`、`runAgentTurnWithFallback`、`src/agents/abort.ts`

---

## 本阶段要建立的目录结构

```
goclaw/
└── internal/
    ├── ai/              ← 修改：统一 Client 接口，增加模型注册表
    ├── channel/         ← 不变
    ├── session/         ← 不变
    ├── config/          ← 不变
    ├── gateway/         ← 修改：chat.send 通过 AgentRegistry 执行
    └── agent/           ← 新增（核心）
        ├── agent.go     # Agent 结构体、配置解析
        ├── runner.go    # runReplyAgent → runWithFallback → runAttempt
        ├── abort.go     # AbortRegistry：run_id → cancel 映射
        └── registry.go  # AgentRegistry：管理多个 Agent 实例
```

---

## 核心概念：执行链

OpenClaw 的代理执行是一个三层调用链，每层有独立职责：

```
runReplyAgent()               ← 第 1 层：用户体验层
  管理 typing indicator        （让用户感知到"正在思考"）
  注册 AbortRegistry
  汇总最终结果写入 Session

  ↓ 调用

runWithFallback()             ← 第 2 层：可靠性层
  按顺序尝试模型列表           （主模型 → fallback 模型）
  区分可重试/不可重试错误

  ↓ 调用（可能多次）

runAttempt()                  ← 第 3 层：执行层
  向具体模型 API 发起请求
  返回流式 channel
```

---

## 第一步：AI 模型注册表

现在 AI 客户端从配置动态创建，不再硬编码。

```go
// internal/ai/registry.go

package ai

import (
    "fmt"
    "sync"
)

// ModelFactory 是创建 Client 的工厂函数
type ModelFactory func(apiKey, model string) Client

var (
    mu        sync.RWMutex
    factories = map[string]ModelFactory{}
)

// RegisterProvider 注册一个模型提供商的工厂（在各提供商包的 init() 中调用）
func RegisterProvider(provider string, f ModelFactory) {
    mu.Lock()
    defer mu.Unlock()
    factories[provider] = f
}

// NewClient 根据提供商名称创建客户端
func NewClient(provider, apiKey, model string) (Client, error) {
    mu.RLock()
    f, ok := factories[provider]
    mu.RUnlock()
    if !ok {
        return nil, fmt.Errorf("unknown AI provider %q", provider)
    }
    return f(apiKey, model), nil
}
```

```go
// internal/ai/openrouter/openrouter.go（已在 Phase 1 的 init() 中注册）
// ai.RegisterProvider("openrouter", ...) 已由 init() 完成，无需改动
```

---

## 第二步：AbortRegistry

```go
// internal/agent/abort.go

package agent

import (
    "context"
    "sync"
)

// AbortRegistry 维护 runID → cancel 的映射
// 允许外部（如 chat.abort RPC 方法）中止正在进行的推理
type AbortRegistry struct {
    mu      sync.Mutex
    cancels map[string]context.CancelFunc
}

func NewAbortRegistry() *AbortRegistry {
    return &AbortRegistry{
        cancels: make(map[string]context.CancelFunc),
    }
}

// Register 为一个运行注册可中止的 context
// 返回的 ctx 是子 context，runID 对应的 cancel 会存入注册表
// 注意：调用方必须在运行结束后调用 Unregister，否则会泄漏
func (r *AbortRegistry) Register(parent context.Context, runID string) (context.Context, context.CancelFunc) {
    ctx, cancel := context.WithCancel(parent)
    r.mu.Lock()
    r.cancels[runID] = cancel
    r.mu.Unlock()
    return ctx, cancel
}

// Unregister 清理已完成的运行记录
func (r *AbortRegistry) Unregister(runID string) {
    r.mu.Lock()
    delete(r.cancels, runID)
    r.mu.Unlock()
}

// Abort 中止指定 runID 的运行
// 返回 true 表示成功中止，false 表示 runID 不存在（已完成或从未存在）
func (r *AbortRegistry) Abort(runID string) bool {
    r.mu.Lock()
    cancel, ok := r.cancels[runID]
    r.mu.Unlock()
    if ok {
        cancel()
    }
    return ok
}

// ActiveRuns 返回当前正在运行的 runID 列表
func (r *AbortRegistry) ActiveRuns() []string {
    r.mu.Lock()
    defer r.mu.Unlock()
    ids := make([]string, 0, len(r.cancels))
    for id := range r.cancels {
        ids = append(ids, id)
    }
    return ids
}
```

---

## 第三步：Agent 结构与执行链

```go
// internal/agent/agent.go

package agent

import (
    "github.com/yourname/goclaw/internal/ai"
    "github.com/yourname/goclaw/internal/channel"
    "github.com/yourname/goclaw/internal/config"
    "github.com/yourname/goclaw/internal/session"
)

// Agent 是 AI 代理的核心结构
// 一个 Agent 对应配置文件中 agents 列表里的一个条目
type Agent struct {
    id           string
    systemPrompt string
    models       []ModelRef  // 主模型 + fallback 模型列表，按优先级排序
    store        session.Store
    abortReg     *AbortRegistry
    channelMgr   *channel.Manager
}

// ModelRef 描述一个模型引用（提供商 + API Key + 模型名）
type ModelRef struct {
    Provider string
    APIKey   string
    Model    string
}

// FromConfig 根据配置创建 Agent
func FromConfig(agentCfg config.AgentConfig, globalAI config.AIConfig, store session.Store, abortReg *AbortRegistry, chanMgr *channel.Manager) *Agent {
    // 合并全局配置和 Agent 级别覆盖
    model := agentCfg.Model
    if model == "" {
        model = globalAI.Model
    }
    systemPrompt := agentCfg.SystemPrompt
    if systemPrompt == "" {
        systemPrompt = globalAI.SystemPrompt
    }

    // 构建模型列表：主模型 + fallback
    var models []ModelRef
    models = append(models, ModelRef{
        Provider: globalAI.Provider,
        APIKey:   globalAI.APIKey,
        Model:    model,
    })
    for _, fb := range agentCfg.Fallback {
        models = append(models, ModelRef{
            Provider: globalAI.Provider,
            APIKey:   globalAI.APIKey,
            Model:    fb,
        })
    }

    return &Agent{
        id:           agentCfg.ID,
        systemPrompt: systemPrompt,
        models:       models,
        store:        store,
        abortReg:     abortReg,
        channelMgr:   chanMgr,
    }
}
```

```go
// internal/agent/runner.go

package agent

import (
    "context"
    "errors"
    "fmt"
    "log"
    "net/http"
    "strings"

    "github.com/yourname/goclaw/internal/ai"
    "github.com/yourname/goclaw/internal/channel"
    "github.com/yourname/goclaw/internal/session"
)

// RunResult 是 Agent 一次运行的结果
type RunResult struct {
    RunID string
    Reply string
    Model string // 实际使用的模型
}

// RunReply 对应 OpenClaw 的 runReplyAgent()
// 这是外部调用的入口，处理用户体验层面的事务
func (a *Agent) RunReply(
    parentCtx context.Context,
    sess *session.Session,
    userText string,
    runID string,
    eventCh chan<- AgentEvent, // 向 Gateway 发送实时事件
) (*RunResult, error) {
    // 1. 注册可中止的 context
    ctx, cancel := a.abortReg.Register(parentCtx, runID)
    defer func() {
        cancel()
        a.abortReg.Unregister(runID)
    }()

    // 2. 发送 typing indicator（如果渠道支持）
    if ch, err := a.channelMgr.Get(sess.Key.ChannelID, sess.Key.AccountID); err == nil {
        if ti, ok := ch.(channel.TypingIndicator); ok {
            go func() {
                // 持续发送 typing，每 4 秒一次（Telegram 的 typing 状态持续 5 秒）
                ticker := time.NewTicker(4 * time.Second)
                defer ticker.Stop()
                ti.SendTyping(ctx, sess.Key.PeerID)
                for {
                    select {
                    case <-ticker.C:
                        ti.SendTyping(ctx, sess.Key.PeerID)
                    case <-ctx.Done():
                        return
                    }
                }
            }()
        }
    }

    // 3. 添加用户消息到会话
    sess.AddUserMessage(userText)

    // 4. 执行推理（带 fallback）
    result, err := a.runWithFallback(ctx, sess, runID, eventCh)
    if err != nil {
        // 如果是用户主动中止，不算错误
        if errors.Is(err, context.Canceled) && ctx.Err() != nil {
            return nil, ErrAborted
        }
        return nil, err
    }

    // 5. 保存回复到会话
    sess.AddAssistantMessage(result.Reply)
    if err := a.store.Save(sess); err != nil {
        log.Printf("[agent:%s] save session error: %v", a.id, err)
    }

    return result, nil
}

// ErrAborted 表示推理被用户主动中止
var ErrAborted = errors.New("inference aborted")

// runWithFallback 对应 OpenClaw 的 runAgentTurnWithFallback()
// 按优先级尝试模型列表，直到成功或全部失败
func (a *Agent) runWithFallback(
    ctx context.Context,
    sess *session.Session,
    runID string,
    eventCh chan<- AgentEvent,
) (*RunResult, error) {
    var lastErr error

    for i, modelRef := range a.models {
        if i > 0 {
            log.Printf("[agent:%s] trying fallback model: %s", a.id, modelRef.Model)
            // 通知 Gateway 正在切换模型
            sendEvent(eventCh, AgentEvent{
                Type:  "agent.model_fallback",
                RunID: runID,
                Data:  map[string]string{"model": modelRef.Model},
            })
        }

        result, err := a.runAttempt(ctx, modelRef, sess, runID, eventCh)
        if err == nil {
            return result, nil
        }

        // ctx 取消（用户中止）→ 立即停止，不尝试 fallback
        if ctx.Err() != nil {
            return nil, err
        }

        // 判断错误是否值得重试
        if !isRetryable(err) {
            log.Printf("[agent:%s] non-retryable error from %s: %v", a.id, modelRef.Model, err)
            return nil, err
        }

        log.Printf("[agent:%s] model %s failed: %v", a.id, modelRef.Model, err)
        lastErr = err
    }

    return nil, fmt.Errorf("all models failed, last error: %w", lastErr)
}

// runAttempt 对应 OpenClaw 的 runEmbeddedAttempt()
// 向单个模型发起一次推理请求
func (a *Agent) runAttempt(
    ctx context.Context,
    modelRef ModelRef,
    sess *session.Session,
    runID string,
    eventCh chan<- AgentEvent,
) (*RunResult, error) {
    client, err := ai.NewClient(modelRef.Provider, modelRef.APIKey, modelRef.Model)
    if err != nil {
        return nil, err
    }

    messages := sess.MessagesForAI(a.systemPrompt, 20)
    textCh, errCh := client.StreamChat(ctx, messages)

    var buf strings.Builder
    for {
        select {
        case chunk, ok := <-textCh:
            if !ok {
                // 流正常结束
                return &RunResult{
                    RunID: runID,
                    Reply: buf.String(),
                    Model: modelRef.Model,
                }, nil
            }
            buf.WriteString(chunk)
            // 发送流式文本块事件给 Gateway
            sendEvent(eventCh, AgentEvent{
                Type:  "chat.delta",
                RunID: runID,
                Data:  map[string]string{"chunk": chunk},
            })

        case err := <-errCh:
            if err != nil {
                return nil, err
            }

        case <-ctx.Done():
            return nil, ctx.Err()
        }
    }
}

// isRetryable 判断错误是否值得切换模型重试
// 限流（429）、服务不可用（503）值得重试
// 参数错误（400）、认证失败（401）不值得重试
func isRetryable(err error) bool {
    var httpErr *HTTPError
    if errors.As(err, &httpErr) {
        return httpErr.StatusCode == http.StatusTooManyRequests ||
            httpErr.StatusCode == http.StatusServiceUnavailable ||
            httpErr.StatusCode >= 500
    }
    // 网络错误（超时、连接重置）值得重试
    return errors.Is(err, context.DeadlineExceeded)
}

// AgentEvent 是 Agent 向 Gateway 发送的实时事件
type AgentEvent struct {
    Type  string
    RunID string
    Data  any
}

func sendEvent(ch chan<- AgentEvent, event AgentEvent) {
    if ch == nil {
        return
    }
    select {
    case ch <- event:
    default:
        // 事件 channel 满了，丢弃（不能阻塞推理）
    }
}

// HTTPError 携带 HTTP 状态码的错误类型
type HTTPError struct {
    StatusCode int
    Message    string
}

func (e *HTTPError) Error() string {
    return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Message)
}
```

---

## 第四步：AgentRegistry

```go
// internal/agent/registry.go

package agent

import (
    "fmt"
    "sync"

    "github.com/yourname/goclaw/internal/channel"
    "github.com/yourname/goclaw/internal/config"
    "github.com/yourname/goclaw/internal/session"
)

// Registry 管理所有配置的 Agent 实例
type Registry struct {
    mu       sync.RWMutex
    agents   map[string]*Agent
    abortReg *AbortRegistry
}

func NewRegistry(
    cfgMgr *config.Manager,
    store session.Store,
    chanMgr *channel.Manager,
) *Registry {
    r := &Registry{
        agents:   make(map[string]*Agent),
        abortReg: NewAbortRegistry(),
    }

    // 初始化时加载所有 Agent
    r.reloadAgents(cfgMgr.Get(), store, chanMgr)

    // 配置变更时重新加载 Agent（热更新，不重启 Gateway）
    cfgMgr.OnChange(func(old, new *config.Config) {
        r.reloadAgents(new, store, chanMgr)
    })

    return r
}

func (r *Registry) reloadAgents(cfg *config.Config, store session.Store, chanMgr *channel.Manager) {
    newAgents := make(map[string]*Agent, len(cfg.Agents))
    for _, agentCfg := range cfg.Agents {
        newAgents[agentCfg.ID] = FromConfig(agentCfg, cfg.AI, store, r.abortReg, chanMgr)
    }
    // 如果没有配置 agents，创建默认代理
    if len(newAgents) == 0 {
        defaultCfg := config.AgentConfig{ID: "default"}
        newAgents["default"] = FromConfig(defaultCfg, cfg.AI, store, r.abortReg, chanMgr)
    }

    r.mu.Lock()
    r.agents = newAgents
    r.mu.Unlock()
}

// Get 根据 agentID 获取 Agent
func (r *Registry) Get(agentID string) (*Agent, error) {
    r.mu.RLock()
    defer r.mu.RUnlock()

    a, ok := r.agents[agentID]
    if !ok {
        return nil, fmt.Errorf("agent %q not found", agentID)
    }
    return a, nil
}

// Abort 中止指定 run_id 的推理
func (r *Registry) Abort(runID string) bool {
    return r.abortReg.Abort(runID)
}

// ActiveRuns 返回所有正在运行的 run_id 列表
func (r *Registry) ActiveRuns() []string {
    return r.abortReg.ActiveRuns()
}
```

---

## 第五步：更新 Gateway 的 chat.send 方法

```go
// internal/gateway/methods/chat.go（重写）

type ChatHandler struct {
    agentRegistry *agent.Registry
    channelMgr    *channel.Manager
    store         session.Store
    hub           *gateway.Hub
}

func (h *ChatHandler) Send(ctx context.Context, raw json.RawMessage) (any, error) {
    var p struct {
        SessionKey string `json:"session_key"`
        Text       string `json:"text"`
        RunID      string `json:"run_id"`
        AgentID    string `json:"agent_id"` // 可选，空=使用 session key 中的 agentID
    }
    if err := json.Unmarshal(raw, &p); err != nil {
        return nil, gateway.NewRPCErr(gateway.ErrBadParams, err.Error())
    }

    key, err := session.Parse(p.SessionKey)
    if err != nil {
        return nil, gateway.NewRPCErr(gateway.ErrBadParams, err.Error())
    }

    agentID := p.AgentID
    if agentID == "" {
        agentID = key.AgentID
    }

    ag, err := h.agentRegistry.Get(agentID)
    if err != nil {
        return nil, gateway.NewRPCErr(gateway.ErrNotFound, err.Error())
    }

    sess, err := h.store.Get(key)
    if err != nil {
        return nil, err
    }

    // Agent 事件转发到 WebSocket Hub
    eventCh := make(chan agent.AgentEvent, 64)
    go func() {
        for e := range eventCh {
            h.hub.Broadcast(gateway.NewEvent(e.Type, map[string]any{
                "run_id": e.RunID,
                "data":   e.Data,
            }))
        }
    }()

    // 异步执行，立即返回 run_id
    go func() {
        defer close(eventCh)
        result, err := ag.RunReply(ctx, sess, p.Text, p.RunID, eventCh)
        if err != nil {
            if errors.Is(err, agent.ErrAborted) {
                h.hub.Broadcast(gateway.NewEvent("chat.aborted", map[string]string{"run_id": p.RunID}))
            } else {
                h.hub.Broadcast(gateway.NewEvent("chat.error", map[string]any{
                    "run_id":  p.RunID,
                    "message": err.Error(),
                }))
            }
            return
        }
        // 发送到渠道
        h.deliverToChannel(ctx, sess, result.Reply)
        h.hub.Broadcast(gateway.NewEvent("chat.done", map[string]any{
            "run_id": p.RunID,
            "text":   result.Reply,
            "model":  result.Model,
        }))
    }()

    return map[string]string{"run_id": p.RunID, "status": "started"}, nil
}

func (h *ChatHandler) deliverToChannel(ctx context.Context, sess *session.Session, text string) {
    ch, err := h.channelMgr.Get(sess.Key.ChannelID, sess.Key.AccountID)
    if err != nil {
        return
    }
    ch.Send(ctx, channel.OutboundMessage{
        PeerID:    sess.Key.PeerID,
        Text:      text,
        ParseMode: channel.ParseModeMarkdown,
    })
}

func (h *ChatHandler) Abort(ctx context.Context, raw json.RawMessage) (any, error) {
    var p struct{ RunID string `json:"run_id"` }
    json.Unmarshal(raw, &p)
    aborted := h.agentRegistry.Abort(p.RunID)
    return map[string]bool{"aborted": aborted}, nil
}
```

---

## 本阶段核心工程知识点

### 1. context.Context 的正确用法

```
parentCtx（来自 main.go 的系统 ctx）
  ↓ WithCancel
runCtx（每个 run 独立，可被 AbortRegistry.Abort 取消）
  ↓ 传递给
StreamChat / sendTyping / deliverToChannel

当 Abort(runID) 被调用时：
  runCtx 取消 → StreamChat 停止输出 → SendTyping goroutine 退出
```

`context.Context` 的取消是树形传播的：父 ctx 取消，所有子 ctx 自动取消。
这就是为什么用 `context.WithCancel(parentCtx)` 而不是 `context.Background()`。

### 2. 错误分类的重要性

```go
func isRetryable(err error) bool {
    // 429 Too Many Requests → 值得换个模型试
    // 503 Service Unavailable → 值得换个模型试
    // 400 Bad Request → 参数问题，换模型也没用，立即失败
    // 401 Unauthorized → API Key 错误，换模型也没用，立即失败
}
```

不加区分地重试所有错误会导致：
- 用错误的参数无限重试（无效）
- 认证失败的请求重试到限流（帮倒忙）

### 3. 事件 channel 的背压设计

```go
eventCh := make(chan agent.AgentEvent, 64) // buffer=64

func sendEvent(ch chan<- AgentEvent, event AgentEvent) {
    select {
    case ch <- event:
    default:
        // buffer 满了，丢弃事件（不阻塞推理）
    }
}
```

推理不能因为事件消费者慢而被阻塞。丢弃流式 delta 事件影响用户体验，
但比推理卡住更好接受。`chat.done` 事件包含完整文本，不会丢失最终结果。

### 4. Typing Indicator 持续刷新

```go
// Telegram 的 typing 状态只持续 5 秒
// 需要每 4 秒发送一次，直到推理结束
go func() {
    ticker := time.NewTicker(4 * time.Second)
    defer ticker.Stop()
    ti.SendTyping(ctx, peerID)
    for {
        select {
        case <-ticker.C:
            ti.SendTyping(ctx, peerID)
        case <-ctx.Done(): // 推理结束或被中止时停止
            return
        }
    }
}()
```

---

## 与 OpenClaw 的对应关系

| 本阶段代码 | OpenClaw 对应 |
|-----------|---------------|
| `agent.RunReply` | `runReplyAgent()` in `src/agents/pi-embedded-runner/` |
| `agent.runWithFallback` | `runAgentTurnWithFallback()` |
| `agent.runAttempt` | `runEmbeddedAttempt()` |
| `AbortRegistry` | `src/agents/abort.ts` 中的 abort 机制 |
| `AgentRegistry.reloadAgents` | OpenClaw 的 agent 配置热更新 |
| `isRetryable` | OpenClaw 的模型错误分类逻辑 |
| `AgentEvent` | `src/gateway/protocol/` 中的 `ChatEvent`、`AgentEvent` |

---

## 下一阶段预告

目前 Agent 只能生成文字回复。
Phase 7 将加入 **Tools（工具调用）**：AI 可以调用注册的函数（搜索、读文件、执行代码），
通过"工具调用循环"实现多步推理，大幅提升 Agent 的实际能力。
