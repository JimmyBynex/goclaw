# Phase 2 — 会话管理：多用户对话历史 + 文件持久化

> 前置：Phase 1 完成，Bot 能和 AI 单轮对话
> 目标：每个用户拥有独立的持久化对话历史，重启后不丢失
> 对应 OpenClaw 模块：`src/sessions/`、`src/routing/session-key.ts`

---

## 本阶段要建立的目录结构

```
goclaw/
├── main.go                  ← 修改：注入 SessionStore
├── config.yaml              ← 新增：session 配置段
└── internal/
    ├── ai/                  ← 不变
    ├── telegram/            ← 不变
    └── session/             ← 新增
        ├── key.go           # SessionKey 定义与序列化
        ├── session.go       # Session 结构体，消息管理
        └── store.go         # Store 接口 + FileStore 实现
```

---

## 核心概念：SessionKey

**SessionKey** 是 OpenClaw 架构的最核心设计之一。它用多个维度唯一标识一个对话：

```
渠道类型 + 账号ID + 隔离级别 + 对端ID + 代理ID
   telegram  + bot001 +    dm    +  12345  +  main
```

这个设计允许：
- 同一个 Bot 的不同用户拥有完全独立的对话
- 群组里所有人共享同一个对话
- 同一个用户在不同渠道（Telegram vs Discord）拥有独立对话
- Phase 9 接入更多渠道时，路由逻辑完全不需要改变

```go
// internal/session/key.go

package session

import (
    "fmt"
    "strings"
)

// Scope 决定会话的隔离级别
type Scope string

const (
    // ScopeDM：私聊，每个用户独立（最常用）
    ScopeDM Scope = "dm"
    // ScopeGroup：群组共享会话
    ScopeGroup Scope = "group"
    // ScopeGlobal：所有用户共享同一个会话（极少用）
    ScopeGlobal Scope = "global"
)

// SessionKey 唯一标识一个会话
// 设计原则：不可变，可序列化为字符串，可从字符串反序列化
type SessionKey struct {
    ChannelID string // "telegram", "discord", "slack"
    AccountID string // Bot 账号标识（一个渠道可能有多个账号）
    Scope     Scope
    PeerID    string // 私聊时是用户 ID，群组时是群组 ID
    AgentID   string // 使用哪个 AI 代理，Phase 6 前固定为 "default"
}

// String 序列化为文件名安全的字符串
func (k SessionKey) String() string {
    return fmt.Sprintf("%s__%s__%s__%s__%s",
        k.ChannelID, k.AccountID, string(k.Scope), k.PeerID, k.AgentID,
    )
}

// Parse 从字符串反序列化
func Parse(s string) (SessionKey, error) {
    parts := strings.Split(s, "__")
    if len(parts) != 5 {
        return SessionKey{}, fmt.Errorf("invalid session key: %q", s)
    }
    return SessionKey{
        ChannelID: parts[0],
        AccountID: parts[1],
        Scope:     Scope(parts[2]),
        PeerID:    parts[3],
        AgentID:   parts[4],
    }, nil
}

// ForTelegram 从 Telegram 消息构造 SessionKey 的便捷函数
// Phase 5 引入 Channel 抽象后，这个函数会移到 telegram 包内
func ForTelegram(botAccountID string, msg interface{ GetChatID() int64; GetChatType() string }) SessionKey {
    // 这里 msg 仍然是 telegram.Message，Phase 5 前直接用
    return SessionKey{}
}
```

---

## Session 结构体

```go
// internal/session/session.go

package session

import (
    "time"

    "github.com/yourname/goclaw/internal/ai"
)

// Session 代表一个持久化的对话
type Session struct {
    Key       SessionKey `json:"key"`
    Messages  []ai.Message `json:"messages"`
    CreatedAt time.Time  `json:"created_at"`
    UpdatedAt time.Time  `json:"updated_at"`
    // MessageCount 记录总消息数，用于触发重置策略（Phase 4 引入）
    MessageCount int `json:"message_count"`
}

func New(key SessionKey) *Session {
    now := time.Now()
    return &Session{
        Key:       key,
        Messages:  []ai.Message{},
        CreatedAt: now,
        UpdatedAt: now,
    }
}

// AddUserMessage 添加用户消息
func (s *Session) AddUserMessage(text string) {
    s.Messages = append(s.Messages, ai.Message{Role: "user", Content: text})
    s.MessageCount++
    s.UpdatedAt = time.Now()
}

// AddAssistantMessage 添加 AI 回复
func (s *Session) AddAssistantMessage(text string) {
    s.Messages = append(s.Messages, ai.Message{Role: "assistant", Content: text})
    s.UpdatedAt = time.Now()
}

// MessagesForAI 返回发给 AI 的消息列表
// 控制上下文窗口：保留 system prompt + 最近 maxPairs 轮对话
func (s *Session) MessagesForAI(systemPrompt string, maxPairs int) []ai.Message {
    var result []ai.Message

    // system prompt 永远是第一条
    if systemPrompt != "" {
        result = append(result, ai.Message{Role: "system", Content: systemPrompt})
    }

    // 计算需要保留的消息数量
    msgs := s.Messages
    maxMsgs := maxPairs * 2 // 每轮 = user + assistant
    if len(msgs) > maxMsgs {
        // 裁剪旧消息，但保持 user/assistant 配对
        msgs = msgs[len(msgs)-maxMsgs:]
    }

    return append(result, msgs...)
}

// Reset 清空对话历史（保留 key 和时间戳）
func (s *Session) Reset() {
    s.Messages = []ai.Message{}
    s.MessageCount = 0
    s.UpdatedAt = time.Now()
}

// ShouldReset 判断是否需要自动重置（Phase 4 接入配置后使用）
func (s *Session) ShouldReset(maxMessages int, maxIdleHours int) bool {
    if maxMessages > 0 && s.MessageCount >= maxMessages {
        return true
    }
    if maxIdleHours > 0 && time.Since(s.UpdatedAt).Hours() >= float64(maxIdleHours) {
        return true
    }
    return false
}
```

---

## Store 接口与 FileStore 实现

```go
// internal/session/store.go

package session

import (
    "encoding/json"
    "fmt"
    "os"
    "path/filepath"
    "sync"
)

// Store 是会话存储的接口
// 目前只有 FileStore 实现，未来可以换成 SQLite 或 Redis
type Store interface {
    Get(key SessionKey) (*Session, error)
    Save(sess *Session) error
    Delete(key SessionKey) error
    List() ([]SessionKey, error)
}

// FileStore 将每个会话存为一个 JSON 文件
// 文件名 = SessionKey.String() + ".json"
type FileStore struct {
    dir   string          // 存储目录，如 ~/.goclaw/sessions/
    mu    sync.RWMutex
    cache map[string]*Session // 内存缓存，减少磁盘读取
}

func NewFileStore(dir string) (*FileStore, error) {
    if err := os.MkdirAll(dir, 0700); err != nil {
        return nil, fmt.Errorf("create session dir: %w", err)
    }
    return &FileStore{
        dir:   dir,
        cache: make(map[string]*Session),
    }, nil
}

// Get 获取会话，不存在则创建新会话
func (s *FileStore) Get(key SessionKey) (*Session, error) {
    id := key.String()

    // 先查内存缓存
    s.mu.RLock()
    if sess, ok := s.cache[id]; ok {
        s.mu.RUnlock()
        return sess, nil
    }
    s.mu.RUnlock()

    // 缓存未命中，读磁盘
    s.mu.Lock()
    defer s.mu.Unlock()

    // double-check：防止两个 goroutine 同时 miss 后重复加载
    if sess, ok := s.cache[id]; ok {
        return sess, nil
    }

    path := s.pathFor(key)
    data, err := os.ReadFile(path)
    if os.IsNotExist(err) {
        // 不存在 → 创建新会话（不立即写盘，等 Save 调用）
        sess := New(key)
        s.cache[id] = sess
        return sess, nil
    }
    if err != nil {
        return nil, fmt.Errorf("read session %s: %w", id, err)
    }

    var sess Session
    if err := json.Unmarshal(data, &sess); err != nil {
        return nil, fmt.Errorf("parse session %s: %w", id, err)
    }
    s.cache[id] = &sess
    return &sess, nil
}

// Save 将会话写入磁盘
// 使用原子写入：先写 .tmp 文件，再 rename，防止写到一半崩溃导致文件损坏
func (s *FileStore) Save(sess *Session) error {
    s.mu.Lock()
    s.cache[sess.Key.String()] = sess // 更新缓存
    s.mu.Unlock()

    data, err := json.MarshalIndent(sess, "", "  ")
    if err != nil {
        return fmt.Errorf("marshal session: %w", err)
    }

    path := s.pathFor(sess.Key)
    tmp := path + ".tmp"

    // 写临时文件
    if err := os.WriteFile(tmp, data, 0600); err != nil {
        return fmt.Errorf("write session tmp: %w", err)
    }

    // 原子 rename（同一文件系统上 rename 是原子操作）
    if err := os.Rename(tmp, path); err != nil {
        os.Remove(tmp) // 清理临时文件
        return fmt.Errorf("rename session: %w", err)
    }

    return nil
}

// Delete 删除会话（清空对话历史）
func (s *FileStore) Delete(key SessionKey) error {
    id := key.String()
    s.mu.Lock()
    delete(s.cache, id)
    s.mu.Unlock()

    path := s.pathFor(key)
    if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
        return err
    }
    return nil
}

// List 列出所有会话键
func (s *FileStore) List() ([]SessionKey, error) {
    entries, err := os.ReadDir(s.dir)
    if err != nil {
        return nil, err
    }
    var keys []SessionKey
    for _, e := range entries {
        if e.IsDir() {
            continue
        }
        name := e.Name()
        // 去掉 .json 后缀
        if len(name) < 5 || name[len(name)-5:] != ".json" {
            continue
        }
        key, err := Parse(name[:len(name)-5])
        if err != nil {
            continue // 跳过不认识的文件
        }
        keys = append(keys, key)
    }
    return keys, nil
}

func (s *FileStore) pathFor(key SessionKey) string {
    return filepath.Join(s.dir, key.String()+".json")
}
```

---

## 修改 config.yaml

```yaml
# config.yaml

telegram:
  token: "YOUR_BOT_TOKEN"
  account_id: "bot001"  # 新增：账号标识，支持多账号

ai:
  provider: "anthropic"
  api_key: "sk-ant-..."
  model: "claude-sonnet-4-6"
  system_prompt: "You are a helpful assistant."
  max_context_pairs: 20  # 新增：最多保留最近 20 轮对话

session:
  dir: "./.goclaw/sessions"  # 新增：会话存储目录
  max_idle_hours: 24         # 24小时无操作自动重置
```

---

## 修改 main.go

在 Phase 1 的 `handler` 函数中注入 `SessionStore`：

```go
// main.go（关键变更部分）

import (
    "context"
    "log"
    "strings"

    "github.com/yourname/goclaw/internal/ai"
    "github.com/yourname/goclaw/internal/ai/anthropic"
    "github.com/yourname/goclaw/internal/session"
    "github.com/yourname/goclaw/internal/telegram"
)

func main() {
    cfg := loadConfig("config.yaml")

    aiClient := anthropic.New(cfg.AI.APIKey, cfg.AI.Model, cfg.AI.SystemPrompt)

    // 初始化会话存储
    store, err := session.NewFileStore(cfg.Session.Dir)
    if err != nil {
        log.Fatalf("init session store: %v", err)
    }

    handler := makeHandler(aiClient, store, cfg)

    bot := telegram.New(cfg.Telegram.Token, handler)
    // ... ctx + StartPolling（与 Phase 1 相同）
}

func makeHandler(aiClient ai.Client, store session.Store, cfg *Config) telegram.MessageHandler {
    return func(ctx context.Context, msg *telegram.Message) (<-chan string, <-chan error) {

        // 1. 构造会话键
        //    私聊：scope=dm，PeerID=用户 ID
        //    群聊：scope=group，PeerID=群组 ID
        scope := session.ScopeDM
        peerID := fmt.Sprintf("%d", msg.From.ID)
        if msg.Chat.Type != "private" {
            scope = session.ScopeGroup
            peerID = fmt.Sprintf("%d", msg.Chat.ID)
        }

        key := session.SessionKey{
            ChannelID: "telegram",
            AccountID: cfg.Telegram.AccountID,
            Scope:     scope,
            PeerID:    peerID,
            AgentID:   "default",
        }

        // 2. 加载或创建会话
        sess, err := store.Get(key)
        if err != nil {
            log.Printf("get session error: %v", err)
            // 降级：创建临时会话，不持久化
            sess = session.New(key)
        }

        // 3. 特殊指令处理
        if strings.TrimSpace(msg.Text) == "/reset" {
            sess.Reset()
            store.Save(sess)
            textCh := make(chan string, 1)
            errCh := make(chan error, 1)
            textCh <- "✅ 对话已重置"
            close(textCh)
            close(errCh)
            return textCh, errCh
        }

        // 4. 添加用户消息
        sess.AddUserMessage(msg.Text)

        // 5. 获取发给 AI 的消息列表（带上下文窗口控制）
        messages := sess.MessagesForAI(cfg.AI.SystemPrompt, cfg.AI.MaxContextPairs)

        // 6. 调用 AI 流式生成
        rawTextCh, errCh := aiClient.StreamChat(ctx, messages)

        // 7. 在流结束后保存回复到会话
        //    需要包装 rawTextCh：消费文本的同时收集完整回复
        textCh := make(chan string, 32)
        go func() {
            defer close(textCh)
            var fullReply strings.Builder
            for chunk := range rawTextCh {
                fullReply.WriteString(chunk)
                textCh <- chunk
            }
            // 流结束后，将完整回复写入会话
            if fullReply.Len() > 0 {
                sess.AddAssistantMessage(fullReply.String())
                if err := store.Save(sess); err != nil {
                    log.Printf("save session error: %v", err)
                }
            }
        }()

        return textCh, errCh
    }
}
```

---

## 运行与验证

```bash
go run main.go

# 验证步骤：
# 1. 发送 "你好"，Bot 回复
# 2. 再发 "我刚才说了什么？"，Bot 应该记得上文
# 3. 发送 /reset，Bot 重置会话
# 4. 再发 "我刚才说了什么？"，Bot 应该说不记得了
# 5. 重启程序，再发 "我刚才说了什么？"，Bot 仍然记得重置前的记录
#    （因为文件持久化了）
```

---

## 本阶段核心工程知识点

### 1. 原子文件写入

```
直接 os.WriteFile(path, data)  ← 危险：写到一半崩溃 = 文件损坏
先写 .tmp → 再 rename          ← 安全：rename 在同一文件系统上是原子操作
```

这和数据库的 WAL（Write-Ahead Log）思想相同：先写到安全的地方，再提交。

### 2. 双重检查锁（Double-Check Lock）

```go
s.mu.RLock()
if sess, ok := s.cache[id]; ok {   // 第一次检查（读锁）
    s.mu.RUnlock()
    return sess, nil
}
s.mu.RUnlock()

s.mu.Lock()
if sess, ok := s.cache[id]; ok {   // 第二次检查（写锁）
    return sess, nil
}
// 确认 miss 后才执行昂贵的磁盘读取
```

防止两个 goroutine 同时 miss 缓存后重复执行磁盘 IO。

### 3. Channel 包装（Tap Pattern）

```go
// 包装 rawTextCh：既传递文本，又收集完整回复
textCh := make(chan string, 32)
go func() {
    defer close(textCh)
    var fullReply strings.Builder
    for chunk := range rawTextCh {  // 从原始 channel 读
        fullReply.WriteString(chunk)
        textCh <- chunk             // 转发给消费方
    }
    // 流结束后执行副作用（保存会话）
    sess.AddAssistantMessage(fullReply.String())
    store.Save(sess)
}()
```

这个模式叫 **Tap**：在数据流中插入一个观察者，不改变数据流本身。

### 4. 上下文窗口管理策略

| 策略 | 优点 | 缺点 |
|------|------|------|
| 截断最旧消息 | 简单 | 丢失早期上下文 |
| 摘要压缩 | 保留语义 | 需要额外 AI 调用 |
| 滑动窗口（本方案） | 简单有效 | 可能破坏对话逻辑 |

Phase 8 的 Memory 系统会提供更优雅的长期记忆方案。

---

## 与 OpenClaw 的对应关系

| 本阶段代码 | OpenClaw 对应 |
|-----------|---------------|
| `SessionKey` | `src/routing/session-key.ts` 的 `SessionKey` |
| `Scope` (dm/group/global) | `dmScope: "per-channel-peer" | "per-channel" | "global"` |
| `FileStore.Save` 原子写入 | OpenClaw 的 session transcript 文件写入 |
| `MessagesForAI` 窗口截断 | `src/context-engine/` 的上下文组装 |
| `/reset` 指令 | `resetPolicy` + 手动 reset 命令 |

---

## 下一阶段预告

Phase 2 的 `handler` 仍然在 `main.go` 里混杂业务逻辑和框架逻辑。
Phase 3 将把"接收消息 → 处理 → 回复"这个流程抽到 **Gateway 服务器**，
并提供 WebSocket RPC 接口，让外部客户端（CLI、移动 App）也能控制系统。
