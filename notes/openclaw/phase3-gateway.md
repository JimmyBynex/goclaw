# Phase 3 — Gateway：WebSocket RPC 服务器 + HTTP 控制面板

> 前置：Phase 2 完成，多用户会话持久化正常
> 目标：构建 WebSocket + HTTP 控制平面，分离框架与业务逻辑
> 对应 OpenClaw 模块：`src/gateway/server.impl.ts`、`src/gateway/server-ws-runtime.ts`、`src/gateway/server-methods/`

---

## 本阶段要建立的目录结构

```
goclaw/
├── main.go              ← 修改：启动 Gateway 而非直接启动 Bot
└── internal/
    ├── ai/              ← 不变
    ├── telegram/        ← 不变
    ├── session/         ← 不变
    └── gateway/         ← 新增
        ├── server.go    # Gateway 主服务，组装所有组件
        ├── protocol.go  # 协议类型：RequestFrame、ResponseFrame、EventFrame
        ├── hub.go       # WebSocket 广播 Hub
        ├── ws.go        # WebSocket 连接处理
        ├── rpc.go       # RPC 路由分发器
        ├── auth.go      # 简单 Token 认证
        └── methods/     # RPC 方法实现
            ├── chat.go  # chat.send / chat.history / chat.abort
            └── health.go # health 探针
```

---

## 第一步：协议设计

协议是系统的契约。设计清晰的协议，才能让 CLI、Web、移动 App 共同使用同一个 Gateway。

```go
// internal/gateway/protocol.go

package gateway

import "encoding/json"

// ── 客户端 → 服务端 ──────────────────────────────────

// RequestFrame 是客户端发来的 RPC 请求
type RequestFrame struct {
    ID     string          `json:"id"`     // 客户端生成的请求 ID，用于匹配响应
    Method string          `json:"method"` // 如 "chat.send"、"health"
    Params json.RawMessage `json:"params"` // 方法参数（各方法自定义结构）
}

// ── 服务端 → 客户端 ──────────────────────────────────

// ResponseFrame 是 RPC 请求的响应（一对一）
type ResponseFrame struct {
    ID    string          `json:"id"`
    Data  json.RawMessage `json:"data,omitempty"`
    Error *RPCError       `json:"error,omitempty"`
}

// EventFrame 是服务端主动推送的事件（一对多，无请求 ID）
// 用于：流式 AI 输出、渠道状态变更、系统通知
type EventFrame struct {
    Type    string          `json:"type"`
    Payload json.RawMessage `json:"payload,omitempty"`
}

// RPCError 遵循 JSON-RPC 风格的错误格式
type RPCError struct {
    Code    string `json:"code"`    // 机器可读的错误码，如 "METHOD_NOT_FOUND"
    Message string `json:"message"` // 人类可读的错误描述
}

// ── 标准错误码 ────────────────────────────────────────

const (
    ErrMethodNotFound = "METHOD_NOT_FOUND"
    ErrBadParams      = "BAD_PARAMS"
    ErrInternalError  = "INTERNAL_ERROR"
    ErrUnauthorized   = "UNAUTHORIZED"
    ErrNotFound       = "NOT_FOUND"
)

// 工具函数

func OKResponse(id string, data any) ResponseFrame {
    raw, _ := json.Marshal(data)
    return ResponseFrame{ID: id, Data: raw}
}

func ErrResponse(id, code, message string) ResponseFrame {
    return ResponseFrame{
        ID:    id,
        Error: &RPCError{Code: code, Message: message},
    }
}

func NewEvent(eventType string, payload any) EventFrame {
    raw, _ := json.Marshal(payload)
    return EventFrame{Type: eventType, Payload: raw}
}
```

---

## 第二步：广播 Hub

Hub 管理所有 WebSocket 客户端连接，负责广播事件。

```go
// internal/gateway/hub.go

package gateway

import (
    "encoding/json"
    "log"
    "sync"
)

// Client 代表一个 WebSocket 连接
type Client struct {
    id   string
    hub  *Hub
    send chan []byte // 待发送给此客户端的消息队列
    // Phase 3 简化版：不做订阅过滤，所有客户端收所有事件
}

// Hub 管理所有客户端连接，提供广播能力
// 使用 channel 序列化操作，避免 map 的并发竞争
type Hub struct {
    clients    map[*Client]bool
    broadcast  chan EventFrame
    register   chan *Client
    unregister chan *Client
    mu         sync.RWMutex
}

func NewHub() *Hub {
    return &Hub{
        clients:    make(map[*Client]bool),
        broadcast:  make(chan EventFrame, 256),
        register:   make(chan *Client),
        unregister: make(chan *Client),
    }
}

// Run 是 Hub 的主循环，必须在独立 goroutine 运行
func (h *Hub) Run() {
    for {
        select {
        case client := <-h.register:
            h.mu.Lock()
            h.clients[client] = true
            h.mu.Unlock()
            log.Printf("[hub] client connected, total: %d", len(h.clients))

        case client := <-h.unregister:
            h.mu.Lock()
            if _, ok := h.clients[client]; ok {
                delete(h.clients, client)
                close(client.send) // 关闭发送 channel，通知 writePump 退出
            }
            h.mu.Unlock()
            log.Printf("[hub] client disconnected, total: %d", len(h.clients))

        case event := <-h.broadcast:
            data, _ := json.Marshal(event)
            h.mu.RLock()
            for client := range h.clients {
                select {
                case client.send <- data:
                    // 成功放入队列
                default:
                    // 客户端的发送队列已满（消费太慢），强制断开
                    // 避免一个慢客户端阻塞整个广播
                    close(client.send)
                    delete(h.clients, client)
                }
            }
            h.mu.RUnlock()
        }
    }
}

// Broadcast 向所有客户端广播事件（非阻塞）
func (h *Hub) Broadcast(event EventFrame) {
    select {
    case h.broadcast <- event:
    default:
        log.Println("[hub] broadcast channel full, dropping event")
    }
}

// ClientCount 返回当前连接的客户端数量
func (h *Hub) ClientCount() int {
    h.mu.RLock()
    defer h.mu.RUnlock()
    return len(h.clients)
}
```

---

## 第三步：RPC 路由分发器

```go
// internal/gateway/rpc.go

package gateway

import (
    "context"
    "encoding/json"
    "log"
)

// HandlerFunc 是 RPC 方法处理函数的类型
// 入参：请求 context + 原始 JSON 参数
// 出参：任意结构（会被序列化为 JSON）+ 错误
type HandlerFunc func(ctx context.Context, params json.RawMessage) (any, error)

// Router 维护方法名到处理函数的映射
type Router struct {
    handlers map[string]HandlerFunc
}

func NewRouter() *Router {
    return &Router{
        handlers: make(map[string]HandlerFunc),
    }
}

// Register 注册一个 RPC 方法
func (r *Router) Register(method string, h HandlerFunc) {
    if _, exists := r.handlers[method]; exists {
        log.Printf("[rpc] warning: overwriting handler for method %q", method)
    }
    r.handlers[method] = h
}

// Dispatch 分发请求到对应处理函数，返回响应帧
func (r *Router) Dispatch(ctx context.Context, frame RequestFrame) ResponseFrame {
    h, ok := r.handlers[frame.Method]
    if !ok {
        return ErrResponse(frame.ID, ErrMethodNotFound, "unknown method: "+frame.Method)
    }

    result, err := h(ctx, frame.Params)
    if err != nil {
        // 区分业务错误和系统错误
        if rpcErr, ok := err.(*RPCErr); ok {
            return ErrResponse(frame.ID, rpcErr.Code, rpcErr.Message)
        }
        return ErrResponse(frame.ID, ErrInternalError, err.Error())
    }

    return OKResponse(frame.ID, result)
}

// RPCErr 是可以携带错误码的业务错误
type RPCErr struct {
    Code    string
    Message string
}

func (e *RPCErr) Error() string { return e.Code + ": " + e.Message }

func NewRPCErr(code, message string) *RPCErr {
    return &RPCErr{Code: code, Message: message}
}
```

---

## 第四步：WebSocket 连接处理

```go
// internal/gateway/ws.go

package gateway

import (
    "context"
    "encoding/json"
    "log"
    "net/http"
    "time"

    "github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
    ReadBufferSize:  4096,
    WriteBufferSize: 4096,
    // 开发阶段允许所有来源，生产环境应该检查 Origin
    CheckOrigin: func(r *http.Request) bool { return true },
}

// ServeWS 将 HTTP 连接升级为 WebSocket
func (g *Gateway) ServeWS(w http.ResponseWriter, r *http.Request) {
    // Token 认证（Phase 3 简化版）
    if !g.auth.Validate(r) {
        http.Error(w, "unauthorized", http.StatusUnauthorized)
        return
    }

    conn, err := upgrader.Upgrade(w, r, nil)
    if err != nil {
        log.Printf("[ws] upgrade failed: %v", err)
        return
    }

    client := &Client{
        id:   generateID(),
        hub:  g.hub,
        send: make(chan []byte, 256),
    }
    g.hub.register <- client

    // 两个 goroutine：一个读，一个写
    // 读和写分开是 WebSocket 的标准模式（conn.ReadMessage 和 conn.WriteMessage 各自线程安全）
    go client.writePump(conn)
    go client.readPump(conn, g.router)
}

// readPump 从 WebSocket 读取请求，分发给 Router
func (c *Client) readPump(conn *websocket.Conn, router *Router) {
    defer func() {
        c.hub.unregister <- c
        conn.Close()
    }()

    conn.SetReadLimit(512 * 1024) // 最大 512KB 的请求
    conn.SetReadDeadline(time.Now().Add(60 * time.Second))
    conn.SetPongHandler(func(string) error {
        conn.SetReadDeadline(time.Now().Add(60 * time.Second))
        return nil
    })

    for {
        _, msg, err := conn.ReadMessage()
        if err != nil {
            if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
                log.Printf("[ws] read error: %v", err)
            }
            return
        }

        var frame RequestFrame
        if err := json.Unmarshal(msg, &frame); err != nil {
            // 发送解析错误响应
            resp := ErrResponse("", ErrBadParams, "invalid JSON: "+err.Error())
            data, _ := json.Marshal(resp)
            c.send <- data
            continue
        }

        // 每个请求在独立 goroutine 处理，支持并发请求
        // context 绑定到连接：连接断开时所有进行中的请求自动取消
        ctx := context.Background() // Phase 6 会加入连接级 cancel
        resp := router.Dispatch(ctx, frame)
        data, _ := json.Marshal(resp)
        c.send <- data
    }
}

// writePump 从 send channel 取数据写入 WebSocket
func (c *Client) writePump(conn *websocket.Conn) {
    ticker := time.NewTicker(54 * time.Second) // Ping 间隔
    defer func() {
        ticker.Stop()
        conn.Close()
    }()

    for {
        select {
        case msg, ok := <-c.send:
            conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
            if !ok {
                // Hub 关闭了 send channel，说明需要断开
                conn.WriteMessage(websocket.CloseMessage, []byte{})
                return
            }
            if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
                return
            }

        case <-ticker.C:
            // 定期发送 Ping，检测连接是否仍然存活
            conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
            if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
                return
            }
        }
    }
}

func generateID() string {
    // 简单实现，Phase 后期换 uuid
    return fmt.Sprintf("%d", time.Now().UnixNano())
}
```

---

## 第五步：RPC 方法实现

### health 方法

```go
// internal/gateway/methods/health.go

package methods

import (
    "context"
    "encoding/json"
    "runtime"
    "time"
)

type HealthHandler struct {
    startTime time.Time
}

func NewHealthHandler() *HealthHandler {
    return &HealthHandler{startTime: time.Now()}
}

func (h *HealthHandler) Health(ctx context.Context, _ json.RawMessage) (any, error) {
    var mem runtime.MemStats
    runtime.ReadMemStats(&mem)
    return map[string]any{
        "ok":        true,
        "uptime":    time.Since(h.startTime).String(),
        "goroutines": runtime.NumGoroutine(),
        "mem_mb":    mem.Alloc / 1024 / 1024,
    }, nil
}
```

### chat 方法

```go
// internal/gateway/methods/chat.go

package methods

import (
    "context"
    "encoding/json"
    "fmt"
    "strings"
    "sync"

    "github.com/yourname/goclaw/internal/ai"
    "github.com/yourname/goclaw/internal/gateway"
    "github.com/yourname/goclaw/internal/session"
)

type ChatHandler struct {
    aiClient  ai.Client
    store     session.Store
    hub       *gateway.Hub
    systemPrompt string

    // 中止注册表（Phase 6 会抽取到独立文件）
    mu      sync.Mutex
    cancels map[string]context.CancelFunc
}

// SendParams 是 chat.send 方法的参数
type SendParams struct {
    SessionKey string `json:"session_key"` // SessionKey 字符串
    Text       string `json:"text"`
    RunID      string `json:"run_id"` // 客户端生成的运行 ID，用于 abort
}

// SendResult 是 chat.send 方法的响应
type SendResult struct {
    RunID   string `json:"run_id"`
    Status  string `json:"status"` // "started"
}

func (h *ChatHandler) Send(ctx context.Context, raw json.RawMessage) (any, error) {
    var p SendParams
    if err := json.Unmarshal(raw, &p); err != nil {
        return nil, gateway.NewRPCErr(gateway.ErrBadParams, err.Error())
    }
    if p.Text == "" {
        return nil, gateway.NewRPCErr(gateway.ErrBadParams, "text is required")
    }

    key, err := session.Parse(p.SessionKey)
    if err != nil {
        return nil, gateway.NewRPCErr(gateway.ErrBadParams, "invalid session_key: "+err.Error())
    }

    sess, err := h.store.Get(key)
    if err != nil {
        return nil, fmt.Errorf("get session: %w", err)
    }

    // 注册可中止的 context
    runCtx, cancel := context.WithCancel(ctx)
    h.mu.Lock()
    h.cancels[p.RunID] = cancel
    h.mu.Unlock()

    // 异步执行 AI 推理，立即返回 run_id
    go func() {
        defer func() {
            cancel()
            h.mu.Lock()
            delete(h.cancels, p.RunID)
            h.mu.Unlock()
        }()

        sess.AddUserMessage(p.Text)
        messages := sess.MessagesForAI(h.systemPrompt, 20)
        textCh, errCh := h.aiClient.StreamChat(runCtx, messages)

        var fullReply strings.Builder
        for {
            select {
            case chunk, ok := <-textCh:
                if !ok {
                    // 流结束
                    reply := fullReply.String()
                    sess.AddAssistantMessage(reply)
                    h.store.Save(sess)
                    // 广播完成事件
                    h.hub.Broadcast(gateway.NewEvent("chat.done", map[string]string{
                        "run_id": p.RunID,
                        "text":   reply,
                    }))
                    return
                }
                fullReply.WriteString(chunk)
                // 广播流式文本块事件
                h.hub.Broadcast(gateway.NewEvent("chat.delta", map[string]string{
                    "run_id": p.RunID,
                    "chunk":  chunk,
                }))

            case err := <-errCh:
                if err != nil {
                    h.hub.Broadcast(gateway.NewEvent("chat.error", map[string]string{
                        "run_id":  p.RunID,
                        "message": err.Error(),
                    }))
                }
                return
            }
        }
    }()

    return SendResult{RunID: p.RunID, Status: "started"}, nil
}

// AbortParams 是 chat.abort 方法的参数
type AbortParams struct {
    RunID string `json:"run_id"`
}

func (h *ChatHandler) Abort(ctx context.Context, raw json.RawMessage) (any, error) {
    var p AbortParams
    json.Unmarshal(raw, &p)

    h.mu.Lock()
    cancel, ok := h.cancels[p.RunID]
    h.mu.Unlock()

    if ok {
        cancel()
    }
    return map[string]bool{"aborted": ok}, nil
}

// HistoryParams 是 chat.history 方法的参数
type HistoryParams struct {
    SessionKey string `json:"session_key"`
}

func (h *ChatHandler) History(ctx context.Context, raw json.RawMessage) (any, error) {
    var p HistoryParams
    json.Unmarshal(raw, &p)
    key, err := session.Parse(p.SessionKey)
    if err != nil {
        return nil, gateway.NewRPCErr(gateway.ErrBadParams, err.Error())
    }
    sess, err := h.store.Get(key)
    if err != nil {
        return nil, err
    }
    return sess.Messages, nil
}
```

---

## 第六步：Gateway 主服务

```go
// internal/gateway/server.go

package gateway

import (
    "context"
    "fmt"
    "log"
    "net/http"

    "github.com/yourname/goclaw/internal/ai"
    "github.com/yourname/goclaw/internal/gateway/methods"
    "github.com/yourname/goclaw/internal/session"
)

type Gateway struct {
    hub    *Hub
    router *Router
    auth   *Auth
    server *http.Server
}

type Config struct {
    Port         int
    Token        string // 简单 Bearer Token 认证
    SystemPrompt string
}

func New(cfg Config, aiClient ai.Client, store session.Store) *Gateway {
    hub := NewHub()
    router := NewRouter()

    g := &Gateway{
        hub:    hub,
        router: router,
        auth:   NewAuth(cfg.Token),
    }

    // 注册 RPC 方法
    health := methods.NewHealthHandler()
    router.Register("health", health.Health)

    chat := methods.NewChatHandler(aiClient, store, hub, cfg.SystemPrompt)
    router.Register("chat.send", chat.Send)
    router.Register("chat.abort", chat.Abort)
    router.Register("chat.history", chat.History)

    // HTTP 路由
    mux := http.NewServeMux()
    mux.HandleFunc("/ws", g.ServeWS)              // WebSocket RPC
    mux.HandleFunc("/health", g.ServeHealthHTTP)  // HTTP 健康探针（不需要认证）
    mux.HandleFunc("/", g.ServeDashboard)          // 简单控制面板

    g.server = &http.Server{
        Addr:    fmt.Sprintf(":%d", cfg.Port),
        Handler: mux,
    }

    return g
}

func (g *Gateway) Start(ctx context.Context) error {
    // 启动 Hub
    go g.hub.Run()

    // 优雅关闭：ctx 取消时关闭 HTTP 服务器
    go func() {
        <-ctx.Done()
        log.Println("[gateway] shutting down...")
        g.server.Shutdown(context.Background())
    }()

    log.Printf("[gateway] listening on %s", g.server.Addr)
    if err := g.server.ListenAndServe(); err != http.ErrServerClosed {
        return err
    }
    return nil
}

// ServeHealthHTTP 是 HTTP 健康探针（给 Docker/k8s 用）
func (g *Gateway) ServeHealthHTTP(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")
    fmt.Fprintf(w, `{"ok":true,"clients":%d}`, g.hub.ClientCount())
}

// ServeDashboard 是简单的文本控制面板
func (g *Gateway) ServeDashboard(w http.ResponseWriter, r *http.Request) {
    fmt.Fprintf(w, "goclaw gateway running\nclients: %d\n", g.hub.ClientCount())
}
```

### 简单 Token 认证

```go
// internal/gateway/auth.go

package gateway

import (
    "net/http"
    "strings"
)

type Auth struct {
    token string
}

func NewAuth(token string) *Auth {
    return &Auth{token: token}
}

// Validate 检查请求头中的 Bearer Token
// WebSocket 升级请求通过 ?token=xxx query param 传递
func (a *Auth) Validate(r *http.Request) bool {
    if a.token == "" {
        return true // 未配置 token 时，允许所有请求（开发模式）
    }

    // 优先检查 Authorization header
    auth := r.Header.Get("Authorization")
    if strings.HasPrefix(auth, "Bearer ") {
        return strings.TrimPrefix(auth, "Bearer ") == a.token
    }

    // 其次检查 query param（WebSocket 握手时用）
    return r.URL.Query().Get("token") == a.token
}
```

---

## 修改 main.go

```go
// main.go

func main() {
    cfg := loadConfig("config.yaml")

    aiClient := anthropic.New(cfg.AI.APIKey, cfg.AI.Model, cfg.AI.SystemPrompt)
    store, _ := session.NewFileStore(cfg.Session.Dir)

    // 启动 Gateway
    gw := gateway.New(gateway.Config{
        Port:         cfg.Gateway.Port,
        Token:        cfg.Gateway.Token,
        SystemPrompt: cfg.AI.SystemPrompt,
    }, aiClient, store)

    // Telegram Bot 仍然运行，但现在它把消息路由给 Gateway 的内部逻辑
    // Phase 5 引入 Channel 抽象后，Bot 会完全解耦
    // ...

    ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer cancel()

    if err := gw.Start(ctx); err != nil {
        log.Fatal(err)
    }
}
```

---

## 用 wscat 测试 Gateway

```bash
# 安装 wscat
npm install -g wscat

# 连接（开发模式无 token）
wscat -c ws://localhost:18789/ws

# 发送 health 请求
{"id":"1","method":"health","params":{}}

# 期望响应：
{"id":"1","data":{"ok":true,"uptime":"5m3s","goroutines":8,"mem_mb":12}}

# 发送聊天请求
{"id":"2","method":"chat.send","params":{"session_key":"telegram__bot001__dm__12345__default","text":"你好","run_id":"run-001"}}

# 期望先收到响应：
{"id":"2","data":{"run_id":"run-001","status":"started"}}

# 然后持续收到事件：
{"type":"chat.delta","payload":{"run_id":"run-001","chunk":"你好"}}
{"type":"chat.delta","payload":{"run_id":"run-001","chunk":"！有什么"}}
...
{"type":"chat.done","payload":{"run_id":"run-001","text":"你好！有什么可以帮助你的？"}}
```

---

## 本阶段核心工程知识点

### 1. 请求-响应 vs 事件推送的分离

```
RPC 请求 (chat.send)  → 立即返回 run_id（异步启动）
AI 流式输出            → 通过 EventFrame 广播 chat.delta 事件
完成                   → 广播 chat.done 事件
```

这和 HTTP/2 的 Server Push 思想相同：控制信道和数据信道分离。

### 2. writePump/readPump 模式

WebSocket 连接的标准 Go 写法：
- `readPump`：一个 goroutine 专门读，阻塞在 `conn.ReadMessage()`
- `writePump`：一个 goroutine 专门写，从 channel 取数据写入
- 两者通过 channel 通信，不共享 conn 的并发写入（WebSocket conn 写不是并发安全的）

### 3. 背压（Backpressure）处理

```go
select {
case client.send <- data:
    // 成功
default:
    // 队列满了，丢弃并断开慢客户端
    // 宁可断开一个慢客户端，也不能让它拖慢所有客户端的广播
}
```

### 4. 优雅关闭顺序

```
ctx 取消
  → HTTP 服务器 Shutdown（等待进行中的请求完成）
  → Hub 感知到客户端断开（writePump 退出触发）
  → Telegram Bot 停止轮询（ctx.Done() 触发）
```

---

## 与 OpenClaw 的对应关系

| 本阶段代码 | OpenClaw 对应 |
|-----------|---------------|
| `RequestFrame` / `ResponseFrame` | `src/gateway/protocol/index.ts` 的 `GatewayFrame` |
| `EventFrame` | `src/gateway/protocol/index.ts` 的 `EventFrame` |
| `Hub.Broadcast` | `server-ws-runtime.ts` 的广播逻辑 |
| `Router.Dispatch` | `server-methods.ts` 的 `coreGatewayHandlers` |
| `chat.send` 异步执行 + 事件推送 | `server-chat.ts` 的 chat 分发 |
| `chat.abort` | `server-methods/chat.ts` 的 abort 逻辑 |
| `Auth.Validate` | `src/gateway/auth.ts` |

---

## 下一阶段预告

Phase 3 的配置直接硬编码在 `loadConfig` 里，没有热重载。
Phase 4 将构建完整的**配置系统**：YAML 结构化配置、`fsnotify` 文件监听、
运行时热重载（部分配置不重启服务直接生效）。
