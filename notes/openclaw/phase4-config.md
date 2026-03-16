# Phase 4 — 配置系统：结构化配置 + 热重载

> 前置：Phase 3 完成，Gateway WebSocket RPC 正常运行
> 目标：YAML 结构化配置、运行时热重载、配置验证
> 对应 OpenClaw 模块：`src/config/`、`src/config/manager.ts`、fsnotify 文件监听

---

## 本阶段要建立的目录结构

```
goclaw/
├── config.yaml          ← 重构：添加所有子系统配置段
├── main.go              ← 修改：使用 config.Manager
└── internal/
    ├── ai/              ← 不变
    ├── telegram/        ← 不变
    ├── session/         ← 不变
    ├── gateway/         ← 修改：接受 *config.Config 而非独立参数
    └── config/          ← 新增
        ├── types.go     # 完整配置结构体定义（单一事实来源）
        ├── loader.go    # 从文件加载并验证配置
        └── manager.go   # 热重载管理器（atomic + fsnotify）
```

---

## 第一步：完整配置结构体

所有配置集中在一个文件，作为整个系统的**单一事实来源（Single Source of Truth）**。
Phase 4 之后，其他包通过 `*config.Config` 获取配置，不再各自读 YAML。

```go
// internal/config/types.go

package config

// Config 是整个系统的配置根节点
// 对应 OpenClaw 的 openclaw.json 结构
type Config struct {
    Gateway  GatewayConfig            `yaml:"gateway"`
    Telegram TelegramConfig           `yaml:"telegram"`
    AI       AIConfig                 `yaml:"ai"`
    Session  SessionConfig            `yaml:"session"`
    Agents   []AgentConfig            `yaml:"agents"`
    Bindings []BindingConfig          `yaml:"bindings"`
}

// GatewayConfig 对应 OpenClaw 的 gateway 配置段
type GatewayConfig struct {
    Port   int    `yaml:"port"`   // 默认 18789
    Bind   string `yaml:"bind"`   // "loopback" | "all"
    Token  string `yaml:"token"`  // Bearer Token 认证（空=不鉴权）
    Reload string `yaml:"reload"` // "hybrid" | "hot" | "restart" | "off"
}

// TelegramConfig 对应 channels.telegram 配置段
type TelegramConfig struct {
    Token     string `yaml:"token"`
    AccountID string `yaml:"account_id"`
}

// AIConfig 对应 models 配置段
type AIConfig struct {
    Provider        string   `yaml:"provider"`         // "anthropic" | "openai"
    APIKey          string   `yaml:"api_key"`
    Model           string   `yaml:"model"`
    FallbackModels  []string `yaml:"fallback_models"`  // Phase 6 使用
    SystemPrompt    string   `yaml:"system_prompt"`
    MaxContextPairs int      `yaml:"max_context_pairs"` // 默认 20
    MaxTokens       int      `yaml:"max_tokens"`         // 默认 4096
}

// SessionConfig 对应 session 配置段
type SessionConfig struct {
    Dir          string `yaml:"dir"`            // 会话文件存储目录
    MaxIdleHours int    `yaml:"max_idle_hours"` // 空闲超时自动重置（0=禁用）
    MaxMessages  int    `yaml:"max_messages"`   // 最大消息数自动重置（0=禁用）
}

// AgentConfig 对应 agents 列表中的每个代理
type AgentConfig struct {
    ID           string   `yaml:"id"`
    Model        string   `yaml:"model"`        // 覆盖全局 AI.Model
    SystemPrompt string   `yaml:"system_prompt"` // 覆盖全局 AI.SystemPrompt
    Fallback     []string `yaml:"fallback"`
}

// BindingConfig 对应 bindings 列表（Phase 9 多渠道路由使用）
type BindingConfig struct {
    AgentID   string        `yaml:"agent_id"`
    Match     BindingMatch  `yaml:"match"`
}

type BindingMatch struct {
    Channel   string `yaml:"channel"`    // "telegram" | "discord" | ""（匹配所有）
    AccountID string `yaml:"account_id"` // 空=匹配所有账号
}

// ── 默认值 ────────────────────────────────────────────

// WithDefaults 返回填充了默认值的 Config
func WithDefaults() Config {
    return Config{
        Gateway: GatewayConfig{
            Port:   18789,
            Bind:   "loopback",
            Reload: "hybrid",
        },
        AI: AIConfig{
            Provider:        "anthropic",
            Model:           "claude-sonnet-4-6",
            MaxContextPairs: 20,
            MaxTokens:       4096,
        },
        Session: SessionConfig{
            Dir:          "./.goclaw/sessions",
            MaxIdleHours: 24,
        },
    }
}
```

---

## 第二步：配置加载与验证

```go
// internal/config/loader.go

package config

import (
    "fmt"
    "os"

    "gopkg.in/yaml.v3"
)

// Load 从文件路径加载配置，返回验证后的 Config
// 先应用默认值，再用文件内容覆盖，再验证
func Load(path string) (*Config, error) {
    // 1. 从默认值开始
    cfg := WithDefaults()

    // 2. 读取文件
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, fmt.Errorf("read config file %q: %w", path, err)
    }

    // 3. 解析 YAML（覆盖默认值）
    if err := yaml.Unmarshal(data, &cfg); err != nil {
        return nil, fmt.Errorf("parse config file %q: %w", path, err)
    }

    // 4. 展开环境变量（支持 ${VAR} 语法）
    expandEnv(&cfg)

    // 5. 验证
    if err := validate(&cfg); err != nil {
        return nil, fmt.Errorf("invalid config: %w", err)
    }

    return &cfg, nil
}

// expandEnv 将配置中的 ${ENV_VAR} 替换为实际环境变量值
// 这样 API Key 可以写成 ${ANTHROPIC_API_KEY}，不暴露在配置文件里
func expandEnv(cfg *Config) {
    cfg.Telegram.Token = os.ExpandEnv(cfg.Telegram.Token)
    cfg.AI.APIKey = os.ExpandEnv(cfg.AI.APIKey)       // ${OPENROUTER_API_KEY}
    cfg.Gateway.Token = os.ExpandEnv(cfg.Gateway.Token)
}

// validate 检查配置的合法性
// 原则：快速失败，明确报告错误，不静默忽略
func validate(cfg *Config) error {
    if cfg.Gateway.Port <= 0 || cfg.Gateway.Port > 65535 {
        return fmt.Errorf("gateway.port must be 1-65535, got %d", cfg.Gateway.Port)
    }

    validBindModes := map[string]bool{"loopback": true, "all": true}
    if !validBindModes[cfg.Gateway.Bind] {
        return fmt.Errorf("gateway.bind must be 'loopback' or 'all', got %q", cfg.Gateway.Bind)
    }

    validReloadModes := map[string]bool{"hybrid": true, "hot": true, "restart": true, "off": true}
    if !validReloadModes[cfg.Gateway.Reload] {
        return fmt.Errorf("gateway.reload must be hybrid/hot/restart/off, got %q", cfg.Gateway.Reload)
    }

    if cfg.Telegram.Token == "" {
        return fmt.Errorf("telegram.token is required")
    }

    if cfg.AI.APIKey == "" {
        return fmt.Errorf("ai.api_key is required")
    }

    validProviders := map[string]bool{"openrouter": true, "anthropic": true, "openai": true}
    if !validProviders[cfg.AI.Provider] {
        return fmt.Errorf("ai.provider must be 'openrouter'/'anthropic'/'openai', got %q", cfg.AI.Provider)
    }

    if cfg.Session.Dir == "" {
        return fmt.Errorf("session.dir is required")
    }

    return nil
}
```

---

## 第三步：热重载管理器

这是本阶段最核心的部分，对应 OpenClaw 的配置热重载流程。

```go
// internal/config/manager.go

package config

import (
    "context"
    "log"
    "sync/atomic"
    "unsafe"

    "github.com/fsnotify/fsnotify"
)

// OnChangeFunc 是配置变更时的回调类型
// old 是旧配置，new 是新配置
// 回调在独立 goroutine 中调用，需自行处理并发安全
type OnChangeFunc func(old, new *Config)

// Manager 负责配置的加载和热重载
// 使用 atomic.Pointer 保证 Get() 完全无锁
type Manager struct {
    path     string
    ptr      atomic.Pointer[Config] // Go 1.19+：原子指针，读操作无锁
    watcher  *fsnotify.Watcher
    handlers []OnChangeFunc
}

// NewManager 创建并初始化配置管理器
// 立即加载一次配置，失败则返回错误
func NewManager(path string) (*Manager, error) {
    watcher, err := fsnotify.NewWatcher()
    if err != nil {
        return nil, err
    }
    if err := watcher.Add(path); err != nil {
        watcher.Close()
        return nil, err
    }

    m := &Manager{
        path:    path,
        watcher: watcher,
    }

    // 首次加载
    cfg, err := Load(path)
    if err != nil {
        watcher.Close()
        return nil, err
    }
    m.ptr.Store(cfg)

    return m, nil
}

// Get 返回当前配置快照（无锁，O(1)，可在热路径上频繁调用）
func (m *Manager) Get() *Config {
    return m.ptr.Load()
}

// OnChange 注册配置变更回调
// 必须在 Watch 启动前注册
func (m *Manager) OnChange(fn OnChangeFunc) {
    m.handlers = append(m.handlers, fn)
}

// Watch 启动文件监听，阻塞直到 ctx 取消
func (m *Manager) Watch(ctx context.Context) {
    defer m.watcher.Close()

    for {
        select {
        case event, ok := <-m.watcher.Events:
            if !ok {
                return
            }
            // 只处理写入和创建事件（vim 等编辑器会先删除再创建）
            if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
                m.reload()
            }
            // 文件被删除后重新添加监听（某些编辑器会这样）
            if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
                m.watcher.Add(m.path)
            }

        case err, ok := <-m.watcher.Errors:
            if !ok {
                return
            }
            log.Printf("[config] watcher error: %v", err)

        case <-ctx.Done():
            return
        }
    }
}

func (m *Manager) reload() {
    newCfg, err := Load(m.path)
    if err != nil {
        // 配置有错误：记录日志，保持旧配置继续运行
        // 关键原则：配置文件写到一半时不能让服务崩溃
        log.Printf("[config] reload failed, keeping old config: %v", err)
        return
    }

    old := m.ptr.Swap(newCfg)
    log.Println("[config] reloaded successfully")

    // 通知所有监听者
    for _, fn := range m.handlers {
        go fn(old, newCfg) // 异步回调，不阻塞监听循环
    }
}
```

---

## 第四步：热重载策略的实现

OpenClaw 有 4 种热重载模式：`hybrid`、`hot`、`restart`、`off`。
我们实现 `hybrid`（最实用的模式）：

```go
// internal/config/reload_policy.go

package config

// ReloadDecision 描述某个配置字段变更时应该如何响应
type ReloadDecision int

const (
    // ReloadNone：热更新，直接生效，不需要重启任何组件
    ReloadNone ReloadDecision = iota
    // ReloadChannel：重启受影响的渠道（如 Telegram Token 变了）
    ReloadChannel
    // ReloadGateway：重启整个 Gateway（如端口变了）
    ReloadGateway
    // ReloadAll：重启整个进程
    ReloadAll
)

// Diff 对比两个配置，返回需要的重启级别
func Diff(old, new *Config) ReloadDecision {
    // 端口或绑定模式变化 → 必须重启 Gateway
    if old.Gateway.Port != new.Gateway.Port || old.Gateway.Bind != new.Gateway.Bind {
        return ReloadGateway
    }

    // Telegram Token 变化 → 重启 Telegram 连接
    if old.Telegram.Token != new.Telegram.Token {
        return ReloadChannel
    }

    // AI 配置、Session 配置、系统提示变化 → 热更新，下次对话生效
    // （这些值每次请求时从 Manager.Get() 实时读取）
    return ReloadNone
}
```

在 Gateway 的 `OnChange` 回调中使用：

```go
// 在 Gateway 初始化时注册配置变更处理
cfgManager.OnChange(func(old, new *config.Config) {
    decision := config.Diff(old, new)
    switch decision {
    case config.ReloadNone:
        log.Println("[config] hot update applied")
        // Gateway 持有 *config.Manager，每次请求时调用 Get()，自动获取最新值

    case config.ReloadChannel:
        log.Println("[config] telegram config changed, restarting channel...")
        // Phase 5 引入 ChannelManager 后，这里调用 channelManager.Restart("telegram")

    case config.ReloadGateway:
        log.Println("[config] gateway config changed, restart required")
        // 通知 main.go 重启（通过 channel 信号）
    }
})
```

---

## 第五步：更新 config.yaml

```yaml
# config.yaml

# ── Gateway 控制平面 ───────────────────────────────────
gateway:
  port: 18789
  bind: loopback       # loopback=只监听 127.0.0.1，all=监听所有网卡
  token: ""            # 空=开发模式不鉴权；生产时设置随机 token
  reload: hybrid       # hybrid | hot | restart | off

# ── Telegram 渠道 ─────────────────────────────────────
telegram:
  token: "${TELEGRAM_BOT_TOKEN}"   # 从环境变量读取，不硬编码
  account_id: "bot001"

# ── AI 模型配置 ───────────────────────────────────────
ai:
  provider: openrouter
  api_key: "${OPENROUTER_API_KEY}"   # 从环境变量读取，https://openrouter.ai/keys
  model: anthropic/claude-sonnet-4-6 # OpenRouter 格式：provider/model-name
  fallback_models:                   # Phase 6 使用
    - anthropic/claude-haiku-4-5
  system_prompt: "You are a helpful assistant. Reply in the same language as the user."
  max_context_pairs: 20
  max_tokens: 4096

# ── 会话管理 ──────────────────────────────────────────
session:
  dir: ./.goclaw/sessions
  max_idle_hours: 24    # 24小时不活跃自动重置，0=禁用
  max_messages: 0       # 消息数量上限重置，0=禁用

# ── 代理列表（Phase 6 扩展） ───────────────────────────
agents:
  - id: default
    # model/system_prompt 不填则使用全局 ai 配置

# ── 路由规则（Phase 9 多渠道使用） ────────────────────
bindings:
  - agent_id: default
    match:
      channel: telegram  # 所有 telegram 消息路由到 default 代理
```

---

## 第六步：修改 main.go，使用 Manager

```go
// main.go

package main

import (
    "context"
    "log"
    "os"
    "os/signal"
    "syscall"

    "github.com/yourname/goclaw/internal/ai/anthropic"
    "github.com/yourname/goclaw/internal/config"
    "github.com/yourname/goclaw/internal/gateway"
    "github.com/yourname/goclaw/internal/session"
    "github.com/yourname/goclaw/internal/telegram"
)

func main() {
    cfgPath := "config.yaml"
    if len(os.Args) > 1 {
        cfgPath = os.Args[1]
    }

    // 加载配置（失败立即退出）
    cfgMgr, err := config.NewManager(cfgPath)
    if err != nil {
        log.Fatalf("load config: %v", err)
    }

    cfg := cfgMgr.Get()

    // 初始化 AI 客户端（Phase 6 会根据配置动态选择）
    aiClient := anthropic.New(cfg.AI.APIKey, cfg.AI.Model, cfg.AI.SystemPrompt)

    // 初始化会话存储
    store, err := session.NewFileStore(cfg.Session.Dir)
    if err != nil {
        log.Fatalf("init session store: %v", err)
    }

    // 初始化 Gateway，传入 cfgMgr 而不是固定 Config
    // Gateway 内部每次请求调用 cfgMgr.Get()，自动获取最新配置
    gw := gateway.New(cfgMgr, aiClient, store)

    // 注册配置变更回调
    cfgMgr.OnChange(func(old, new *config.Config) {
        decision := config.Diff(old, new)
        log.Printf("[main] config changed, decision: %v", decision)
        // Phase 5 后这里会触发 ChannelManager 相应操作
    })

    ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer cancel()

    // 启动配置文件监听（后台 goroutine）
    go cfgMgr.Watch(ctx)

    // 启动 Telegram Bot（暂时直接启动，Phase 5 后交由 ChannelManager 管理）
    bot := telegram.New(cfg.Telegram.Token, makeHandler(cfgMgr, aiClient, store))
    go func() {
        if err := bot.StartPolling(ctx); err != nil {
            log.Printf("telegram polling error: %v", err)
        }
    }()

    // 启动 Gateway（阻塞）
    if err := gw.Start(ctx); err != nil {
        log.Fatalf("gateway: %v", err)
    }
}
```

---

## 本阶段核心工程知识点

### 1. `atomic.Pointer` vs `sync.RWMutex`

```go
// 方案 A：RWMutex 包装（传统做法）
type Manager struct {
    mu  sync.RWMutex
    cfg *Config
}
func (m *Manager) Get() *Config {
    m.mu.RLock()
    defer m.mu.RUnlock()
    return m.cfg
}

// 方案 B：atomic.Pointer（本方案，Go 1.19+）
type Manager struct {
    ptr atomic.Pointer[Config]
}
func (m *Manager) Get() *Config {
    return m.ptr.Load() // 无锁，CAS 原语
}
```

`atomic.Pointer` 在高并发读场景（每个请求都调用 `Get()`）性能远优于 RWMutex，
因为 RWMutex 的读锁虽然不排斥其他读，但仍有原子计数操作；`atomic.Pointer.Load()` 是单条 CPU 指令。

### 2. 配置不可变性（Immutable Config）

```go
// ❌ 错误：直接修改 Config
cfg := cfgMgr.Get()
cfg.AI.Model = "new-model" // 这会修改共享的 Config 对象！

// ✅ 正确：Config 是只读快照，修改请创建新对象
// 配置更新只通过 Manager.reload() → atomic.Swap() 进行
```

`atomic.Pointer` 存储的配置指针替换是原子的，读取的调用方始终拿到一个完整的配置快照，不会读到"写到一半"的配置。

### 3. 环境变量注入 vs 明文配置

```yaml
# ❌ 不安全：API Key 写在文件里
api_key: "sk-ant-xxxx"

# ✅ 安全：从环境变量读取
api_key: "${ANTHROPIC_API_KEY}"
```

运行时：
```bash
export OPENROUTER_API_KEY="sk-or-xxxx"
export TELEGRAM_BOT_TOKEN="1234567890:xxx"
./goclaw
```

这对应 OpenClaw 的 Secrets 管理机制：凭证与配置分离。

### 4. vim 的文件操作陷阱

vim 保存文件的实际步骤：
```
1. 写入 file.yaml.swp（临时文件）
2. 删除原 file.yaml
3. 将 .swp 重命名为 file.yaml
```

这会触发 `fsnotify.Remove` 事件，导致监听失效！
解决方案：监听到 Remove 后重新 `watcher.Add(path)`（代码中已处理）。

---

## 与 OpenClaw 的对应关系

| 本阶段代码 | OpenClaw 对应 |
|-----------|---------------|
| `config.Types` | `src/config/types.ts` 的 `OpenClawConfig` |
| `config.Load` | `src/config/loader.ts` 的 load + Zod 验证 |
| `config.Manager` | `src/config/manager.ts` |
| `atomic.Pointer` | TypeScript 用 `let current: Config` + 锁实现 |
| `expandEnv` | OpenClaw 的 Secrets 注入机制 |
| `config.Diff` | `src/gateway/server.impl.ts` 里的 reload mode 判断 |
| `OnChangeFunc` | OpenClaw config `onChange` 回调 |

---

## 下一阶段预告

Phase 4 的 Telegram Bot 仍然直接在 `main.go` 里初始化，和 Gateway 耦合。
Phase 5 将定义 **Channel 接口**，把 Telegram 的具体实现藏在接口后面，
由统一的 **ChannelManager** 管理生命周期，为 Phase 9 接入更多渠道做好准备。
