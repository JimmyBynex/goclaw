# Phase 8 — Memory：长期记忆系统（SQLite + FTS5 + 向量检索）

> 前置：Phase 7 完成，工具调用循环正常
> 目标：AI 可以存储和检索长期记忆，突破上下文窗口限制
> 对应 OpenClaw 模块：`src/context-engine/`、`src/memory/`、builtin backend（SQLite + FTS5 + sqlite-vec）

---

## 本阶段要建立的目录结构

```
goclaw/
└── internal/
    ├── agent/           ← 修改：RunReply 前后自动检索/存储记忆
    └── memory/          ← 新增（核心）
        ├── types.go     # Memory 记录类型、SearchResult 类型
        ├── store.go     # Store 接口定义
        ├── sqlite.go    # SQLite + FTS5 实现（纯文本检索）
        ├── search.go    # 混合检索算法（BM25 + 时间衰减）
        └── manager.go   # MemoryManager：自动记忆提取与注入
```

---

## 核心概念：什么是 Memory？

Session 里的消息是**短期记忆**：有上下文窗口限制，会话重置后消失。
Memory 是**长期记忆**：独立存储，持续存在，通过检索而非完整载入来使用。

```
用户上周说："我在学习 Go 语言，目前在看并发章节"

新对话开始（旧会话已重置）：
用户："帮我写一个 goroutine 示例"

没有 Memory：AI 不知道用户是初学者，可能给出太复杂的例子
有 Memory：检索到"用户正在学 Go 并发" → 给出适合初学者的例子
```

---

## Memory 的生命周期

```
对话发生
  ↓
AI 回复后：MemoryManager 提取关键信息（可以用 AI 提取，也可以规则提取）
  ↓
写入 Memory Store（SQLite）

下次对话时：
用户输入 → 向 Memory Store 检索相关记忆
  ↓
将检索结果注入到 system prompt 或消息历史
  ↓
AI 推理时"记得"这些信息
```

---

## 第一步：类型定义

```go
// internal/memory/types.go

package memory

import "time"

// Entry 是一条记忆记录
type Entry struct {
    ID        int64
    AgentID   string    // 属于哪个 Agent
    SessionID string    // 来自哪个会话（可选，用于追溯）
    Content   string    // 记忆内容（自然语言描述）
    Tags      []string  // 标签（用于过滤）
    Source    string    // 来源："user_message" | "ai_extract" | "manual"
    CreatedAt time.Time
    UpdatedAt time.Time

    // 检索时填充
    Score float64 // 综合相关性分数
}

// SearchQuery 是记忆检索请求
type SearchQuery struct {
    AgentID   string
    Query     string    // 查询文本
    Tags      []string  // 标签过滤（可选）
    Limit     int       // 返回数量上限（默认 5）
    MaxAgeDays int      // 只检索最近 N 天的记忆（0=不限）
}

// Store 是记忆存储的接口
type Store interface {
    // Save 保存一条记忆
    Save(entry *Entry) error

    // Search 检索相关记忆（BM25 + 时间衰减混合排序）
    Search(query SearchQuery) ([]*Entry, error)

    // Delete 删除指定记忆
    Delete(id int64) error

    // List 列出所有记忆（用于管理）
    List(agentID string, limit, offset int) ([]*Entry, error)

    // Count 统计记忆数量
    Count(agentID string) (int64, error)

    // Close 关闭存储
    Close() error
}
```

---

## 第二步：SQLite + FTS5 实现

```go
// internal/memory/sqlite.go

package memory

import (
    "database/sql"
    "encoding/json"
    "fmt"
    "strings"
    "time"

    _ "modernc.org/sqlite" // 纯 Go SQLite，无需 CGO
)

// SQLiteStore 使用 SQLite + FTS5 实现记忆存储
type SQLiteStore struct {
    db *sql.DB
}

func NewSQLiteStore(path string) (*SQLiteStore, error) {
    // DSN 参数：WAL 模式提高并发写入性能
    dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000", path)
    db, err := sql.Open("sqlite", dsn)
    if err != nil {
        return nil, err
    }

    s := &SQLiteStore{db: db}
    if err := s.migrate(); err != nil {
        db.Close()
        return nil, err
    }
    return s, nil
}

// migrate 创建数据库表结构
func (s *SQLiteStore) migrate() error {
    _, err := s.db.Exec(`
        -- 主表：存储记忆元数据
        CREATE TABLE IF NOT EXISTS memories (
            id         INTEGER PRIMARY KEY AUTOINCREMENT,
            agent_id   TEXT NOT NULL,
            session_id TEXT,
            content    TEXT NOT NULL,
            tags       TEXT,          -- JSON 数组
            source     TEXT NOT NULL DEFAULT 'manual',
            created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
            updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
        );

        -- 索引：按 agent_id 查询
        CREATE INDEX IF NOT EXISTS idx_memories_agent ON memories(agent_id);

        -- FTS5 虚拟表：全文检索（BM25 排序）
        -- content=memories 表示 FTS 表的内容来自 memories 表
        -- content_rowid=id 表示 rowid 对应 memories.id
        CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
            content,
            tags,
            content=memories,
            content_rowid=id
        );

        -- 触发器：保持 FTS 表与主表同步
        CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
            INSERT INTO memories_fts(rowid, content, tags)
            VALUES (new.id, new.content, new.tags);
        END;

        CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN
            UPDATE memories_fts
            SET content = new.content, tags = new.tags
            WHERE rowid = new.id;
        END;

        CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
            DELETE FROM memories_fts WHERE rowid = old.id;
        END;
    `)
    return err
}

// Save 保存一条记忆
func (s *SQLiteStore) Save(e *Entry) error {
    tags, _ := json.Marshal(e.Tags)
    now := time.Now()

    result, err := s.db.Exec(`
        INSERT INTO memories (agent_id, session_id, content, tags, source, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?)
    `, e.AgentID, e.SessionID, e.Content, string(tags), e.Source, now, now)
    if err != nil {
        return err
    }

    id, _ := result.LastInsertId()
    e.ID = id
    e.CreatedAt = now
    e.UpdatedAt = now
    return nil
}

// Search 使用 FTS5 BM25 检索记忆，再叠加时间衰减
func (s *SQLiteStore) Search(q SearchQuery) ([]*Entry, error) {
    limit := q.Limit
    if limit <= 0 {
        limit = 5
    }

    // FTS5 的 BM25 函数：分数越小越相关（负数），需要取反
    // bm25(memories_fts, 10, 1) 表示 content 字段权重 10，tags 权重 1
    query := `
        SELECT
            m.id, m.agent_id, m.session_id, m.content, m.tags, m.source,
            m.created_at, m.updated_at,
            -bm25(memories_fts, 10, 1) AS bm25_score
        FROM memories m
        JOIN memories_fts ON memories_fts.rowid = m.id
        WHERE memories_fts MATCH ?
          AND m.agent_id = ?
    `
    args := []any{fts5Query(q.Query), q.AgentID}

    if q.MaxAgeDays > 0 {
        query += " AND m.created_at >= datetime('now', ?)"
        args = append(args, fmt.Sprintf("-%d days", q.MaxAgeDays))
    }

    query += " ORDER BY bm25_score DESC LIMIT ?"
    args = append(args, limit*3) // 多取一些，留给时间衰减重排序

    rows, err := s.db.Query(query, args...)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var entries []*Entry
    for rows.Next() {
        e := &Entry{}
        var tagsJSON string
        var bm25Score float64
        err := rows.Scan(&e.ID, &e.AgentID, &e.SessionID, &e.Content, &tagsJSON,
            &e.Source, &e.CreatedAt, &e.UpdatedAt, &bm25Score)
        if err != nil {
            continue
        }
        json.Unmarshal([]byte(tagsJSON), &e.Tags)
        e.Score = bm25Score
        entries = append(entries, e)
    }

    // 时间衰减重排序
    entries = applyTimeDecay(entries)

    // 截取最终数量
    if len(entries) > limit {
        entries = entries[:limit]
    }
    return entries, nil
}

// fts5Query 将普通查询文本转为 FTS5 查询语法
// FTS5 支持短语查询 "hello world"、前缀匹配 hello* 等
func fts5Query(q string) string {
    words := strings.Fields(q)
    // 对每个词加前缀匹配（*），允许部分匹配
    for i, w := range words {
        words[i] = w + "*"
    }
    return strings.Join(words, " ")
}

func (s *SQLiteStore) Delete(id int64) error {
    _, err := s.db.Exec("DELETE FROM memories WHERE id = ?", id)
    return err
}

func (s *SQLiteStore) List(agentID string, limit, offset int) ([]*Entry, error) {
    rows, err := s.db.Query(`
        SELECT id, agent_id, session_id, content, tags, source, created_at, updated_at
        FROM memories WHERE agent_id = ?
        ORDER BY created_at DESC LIMIT ? OFFSET ?
    `, agentID, limit, offset)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var entries []*Entry
    for rows.Next() {
        e := &Entry{}
        var tagsJSON string
        rows.Scan(&e.ID, &e.AgentID, &e.SessionID, &e.Content, &tagsJSON,
            &e.Source, &e.CreatedAt, &e.UpdatedAt)
        json.Unmarshal([]byte(tagsJSON), &e.Tags)
        entries = append(entries, e)
    }
    return entries, nil
}

func (s *SQLiteStore) Count(agentID string) (int64, error) {
    var count int64
    err := s.db.QueryRow("SELECT COUNT(*) FROM memories WHERE agent_id = ?", agentID).Scan(&count)
    return count, err
}

func (s *SQLiteStore) Close() error { return s.db.Close() }
```

---

## 第三步：时间衰减排序算法

```go
// internal/memory/search.go

package memory

import (
    "math"
    "sort"
    "time"
)

// applyTimeDecay 将 BM25 分数和时间衰减合并，重新排序
//
// 最终分数 = α × BM25归一化分数 + β × 时间衰减系数
// α = 0.7（BM25权重，相关性更重要）
// β = 0.3（时间权重，新记忆优先）
//
// 对应 OpenClaw 的混合排序：α×BM25 + β×向量 - γ×时间衰减
// Phase 8 简化版：不做向量检索，只做 BM25 + 时间衰减
func applyTimeDecay(entries []*Entry) []*Entry {
    if len(entries) == 0 {
        return entries
    }

    const alpha = 0.7 // BM25 权重
    const beta = 0.3  // 时间权重
    const halfLifeDays = 7.0 // 7天后时间分数减半

    // 归一化 BM25 分数到 [0, 1]
    maxBM25 := entries[0].Score
    for _, e := range entries {
        if e.Score > maxBM25 {
            maxBM25 = e.Score
        }
    }

    now := time.Now()
    for _, e := range entries {
        // BM25 归一化分数
        bm25Norm := 0.0
        if maxBM25 > 0 {
            bm25Norm = e.Score / maxBM25
        }

        // 时间衰减分数：指数衰减，halfLifeDays 天后减半
        ageDays := now.Sub(e.CreatedAt).Hours() / 24
        timeScore := math.Exp(-math.Log(2) / halfLifeDays * ageDays)

        // 综合分数
        e.Score = alpha*bm25Norm + beta*timeScore
    }

    // 按综合分数降序排序
    sort.Slice(entries, func(i, j int) bool {
        return entries[i].Score > entries[j].Score
    })

    return entries
}

// MMR（最大边际相关性）去重
// 避免返回内容高度相似的多条记忆
// 在记忆数量很多时使用，Phase 8 可选
func mmrRerank(entries []*Entry, lambda float64, k int) []*Entry {
    if len(entries) <= k {
        return entries
    }

    selected := make([]*Entry, 0, k)
    remaining := make([]*Entry, len(entries))
    copy(remaining, entries)

    // 贪心选择：每次选综合分数最高的，同时惩罚与已选内容相似的
    for len(selected) < k && len(remaining) > 0 {
        bestIdx := 0
        bestScore := -1.0

        for i, candidate := range remaining {
            // 与已选记忆的最大相似度（简单版：基于文字重叠率）
            maxSim := 0.0
            for _, sel := range selected {
                sim := jaccardSimilarity(candidate.Content, sel.Content)
                if sim > maxSim {
                    maxSim = sim
                }
            }
            // MMR 分数 = λ×相关性 - (1-λ)×冗余度
            mmrScore := lambda*candidate.Score - (1-lambda)*maxSim
            if mmrScore > bestScore {
                bestScore = mmrScore
                bestIdx = i
            }
        }

        selected = append(selected, remaining[bestIdx])
        remaining = append(remaining[:bestIdx], remaining[bestIdx+1:]...)
    }

    return selected
}

// jaccardSimilarity 计算两段文本的 Jaccard 相似度（词集合交集/并集）
func jaccardSimilarity(a, b string) float64 {
    setA := tokenize(a)
    setB := tokenize(b)

    intersection := 0
    for w := range setA {
        if setB[w] {
            intersection++
        }
    }
    union := len(setA) + len(setB) - intersection
    if union == 0 {
        return 0
    }
    return float64(intersection) / float64(union)
}

func tokenize(text string) map[string]bool {
    words := strings.Fields(strings.ToLower(text))
    set := make(map[string]bool, len(words))
    for _, w := range words {
        set[w] = true
    }
    return set
}
```

---

## 第四步：MemoryManager（自动提取与注入）

```go
// internal/memory/manager.go

package memory

import (
    "context"
    "fmt"
    "log"
    "strings"

    "github.com/yourname/goclaw/internal/ai"
)

// Manager 负责记忆的自动提取和注入
type Manager struct {
    store     Store
    extractor *ai.Client // 用于 AI 提取记忆（可选，规则提取也可以）
}

func NewManager(store Store) *Manager {
    return &Manager{store: store}
}

// InjectMemories 在 AI 推理前，将相关记忆注入到消息列表
// 注入位置：system prompt 末尾（作为背景知识）
func (m *Manager) InjectMemories(
    ctx context.Context,
    agentID string,
    userInput string,
    messages []ai.Message,
) []ai.Message {
    // 检索相关记忆
    entries, err := m.store.Search(SearchQuery{
        AgentID: agentID,
        Query:   userInput,
        Limit:   5,
    })
    if err != nil || len(entries) == 0 {
        return messages // 无相关记忆，直接返回
    }

    // 构造记忆块文本
    var memBlock strings.Builder
    memBlock.WriteString("\n\n--- Relevant memories ---\n")
    for i, e := range entries {
        memBlock.WriteString(fmt.Sprintf("%d. %s\n", i+1, e.Content))
    }
    memBlock.WriteString("--- End of memories ---")

    // 将记忆注入到 system message
    injected := make([]ai.Message, len(messages))
    copy(injected, messages)

    for i, msg := range injected {
        if msg.Role == "system" {
            injected[i].Content = msg.Content + memBlock.String()
            return injected
        }
    }

    // 没有 system message，在开头插入一条
    return append([]ai.Message{
        {Role: "system", Content: "You are a helpful assistant." + memBlock.String()},
    }, injected...)
}

// ExtractAndSave 在 AI 回复后，提取值得记住的信息
// 简单版本：规则提取（不调用额外的 AI）
// 高级版本：调用 AI 分析对话，提取结构化记忆
func (m *Manager) ExtractAndSave(ctx context.Context, agentID, sessionID, userInput, aiReply string) {
    entries := m.ruleBasedExtract(agentID, sessionID, userInput, aiReply)
    for _, e := range entries {
        if err := m.store.Save(e); err != nil {
            log.Printf("[memory] save failed: %v", err)
        }
    }
}

// ruleBasedExtract 用规则提取值得记忆的信息
// 规则：用户自我介绍、偏好声明、重要决定等
func (m *Manager) ruleBasedExtract(agentID, sessionID, userInput, aiReply string) []*Entry {
    var entries []*Entry
    lower := strings.ToLower(userInput)

    // 规则 1：用户说"我是..."（身份信息）
    if containsAny(lower, "我是", "我叫", "my name is", "i am a") {
        entries = append(entries, &Entry{
            AgentID:   agentID,
            SessionID: sessionID,
            Content:   "用户说：" + userInput,
            Tags:      []string{"identity"},
            Source:    "user_message",
        })
    }

    // 规则 2：用户说"我喜欢/不喜欢..."（偏好）
    if containsAny(lower, "我喜欢", "我不喜欢", "i like", "i prefer", "i hate") {
        entries = append(entries, &Entry{
            AgentID:   agentID,
            SessionID: sessionID,
            Content:   "用户偏好：" + userInput,
            Tags:      []string{"preference"},
            Source:    "user_message",
        })
    }

    // 规则 3：用户说"记住..."（明确要求记忆）
    if containsAny(lower, "记住", "请记住", "remember that", "note that") {
        entries = append(entries, &Entry{
            AgentID:   agentID,
            SessionID: sessionID,
            Content:   strings.TrimPrefix(userInput, "记住"),
            Tags:      []string{"explicit", "important"},
            Source:    "user_message",
        })
    }

    return entries
}

func containsAny(s string, keywords ...string) bool {
    for _, kw := range keywords {
        if strings.Contains(s, kw) {
            return true
        }
    }
    return false
}
```

---

## 第五步：将 Memory 集成到 Agent

修改 `agent/runner.go` 的 `RunReply` 方法：

```go
// internal/agent/runner.go（修改 RunReply）

func (a *Agent) RunReply(
    parentCtx context.Context,
    sess *session.Session,
    userText string,
    runID string,
    eventCh chan<- AgentEvent,
) (*RunResult, error) {
    ctx, cancel := a.abortReg.Register(parentCtx, runID)
    defer func() {
        cancel()
        a.abortReg.Unregister(runID)
    }()

    // typing indicator（同 Phase 6）...

    sess.AddUserMessage(userText)

    // ★ 新增：注入相关记忆到消息历史
    messagesWithMemory := a.memoryMgr.InjectMemories(
        ctx,
        a.id,
        userText,
        sess.MessagesForAI(a.systemPrompt, 20),
    )

    // 用注入了记忆的消息历史执行推理
    result, err := a.runWithFallbackFromMessages(ctx, messagesWithMemory, runID, eventCh)
    if err != nil {
        if errors.Is(err, context.Canceled) && ctx.Err() != nil {
            return nil, ErrAborted
        }
        return nil, err
    }

    // 保存回复到会话
    sess.AddAssistantMessage(result.Reply)
    a.store.Save(sess)

    // ★ 新增：异步提取和保存记忆（不阻塞回复）
    go a.memoryMgr.ExtractAndSave(
        context.Background(), // 不用 ctx，防止被中止
        a.id,
        sess.Key.String(),
        userText,
        result.Reply,
    )

    return result, nil
}
```

---

## 第六步：添加 Memory RPC 方法

在 Gateway 注册 memory 相关 RPC 方法：

```go
// internal/gateway/methods/memory.go

type MemoryHandler struct {
    store   memory.Store
    agentID string
}

// memory.search：搜索记忆（RPC 方法）
func (h *MemoryHandler) Search(ctx context.Context, raw json.RawMessage) (any, error) {
    var p struct {
        AgentID string `json:"agent_id"`
        Query   string `json:"query"`
        Limit   int    `json:"limit"`
    }
    json.Unmarshal(raw, &p)
    if p.Limit <= 0 {
        p.Limit = 10
    }
    entries, err := h.store.Search(memory.SearchQuery{
        AgentID: p.AgentID,
        Query:   p.Query,
        Limit:   p.Limit,
    })
    if err != nil {
        return nil, err
    }
    return entries, nil
}

// memory.save：手动保存一条记忆
func (h *MemoryHandler) Save(ctx context.Context, raw json.RawMessage) (any, error) {
    var e memory.Entry
    if err := json.Unmarshal(raw, &e); err != nil {
        return nil, gateway.NewRPCErr(gateway.ErrBadParams, err.Error())
    }
    if err := h.store.Save(&e); err != nil {
        return nil, err
    }
    return e, nil
}

// memory.delete：删除一条记忆
func (h *MemoryHandler) Delete(ctx context.Context, raw json.RawMessage) (any, error) {
    var p struct{ ID int64 `json:"id"` }
    json.Unmarshal(raw, &p)
    return nil, h.store.Delete(p.ID)
}

// memory.list：列出所有记忆（管理用）
func (h *MemoryHandler) List(ctx context.Context, raw json.RawMessage) (any, error) {
    var p struct {
        AgentID string `json:"agent_id"`
        Limit   int    `json:"limit"`
        Offset  int    `json:"offset"`
    }
    json.Unmarshal(raw, &p)
    if p.Limit <= 0 {
        p.Limit = 20
    }
    return h.store.List(p.AgentID, p.Limit, p.Offset)
}
```

---

## 测试记忆功能

```
# 第一次对话
用户: 记住，我是一个 Go 后端开发者，有 5 年经验
Bot: 好的，我记住了！你是有 5 年经验的 Go 后端开发者。

# 重置会话
用户: /reset
Bot: ✅ 对话已重置

# 第二次对话（新会话，但有记忆）
用户: 给我推荐一些进阶学习资源
Bot: 作为有 5 年经验的 Go 后端开发者，以下进阶资源适合你：
     1. Go 的内存模型和并发深入...
     （AI 从记忆中检索到了用户背景，给出了针对性推荐）
```

---

## 本阶段核心工程知识点

### 1. FTS5 vs LIKE 搜索

```sql
-- ❌ LIKE 搜索：全表扫描，O(n)，不支持相关性排序
SELECT * FROM memories WHERE content LIKE '%Go 语言%'

-- ✅ FTS5：倒排索引，O(log n)，支持 BM25 相关性排序
SELECT * FROM memories_fts WHERE memories_fts MATCH 'Go 语言'
```

FTS5 的 BM25 算法考虑词频（TF）和逆文档频率（IDF），
高频词（如"的"）权重低，低频词（如"goroutine"）权重高。

### 2. WAL 模式（Write-Ahead Log）

```
DSN: file:memory.db?_journal_mode=WAL

普通模式：写入时锁定整个数据库，读操作等待
WAL 模式：写入到 WAL 文件，读操作不阻塞
         适合"频繁读、偶尔写"的记忆检索场景
```

### 3. 时间衰减的数学

```
时间分数 = e^(-ln(2) / halfLife × ageDays)

halfLife=7（7天减半）：
  今天：score = 1.0
  7天前：score = 0.5
  14天前：score = 0.25
  30天前：score = 0.095
```

指数衰减而非线性衰减，是因为"最近"的权重下降应该比"很久以前"的下降更快。

### 4. 异步记忆提取

```go
// ✅ 异步：不阻塞回复
go a.memoryMgr.ExtractAndSave(context.Background(), ...)

// ❌ 同步：用户要等记忆提取完才收到回复
a.memoryMgr.ExtractAndSave(ctx, ...)
```

使用 `context.Background()` 而不是推理的 `ctx`：
即使推理被中止，也要保存记忆（用户的话已经说了，值得记住）。

---

## 与 OpenClaw 的对应关系

| 本阶段代码 | OpenClaw 对应 |
|-----------|---------------|
| `memory.Store` 接口 | `src/memory/` 的后端接口 |
| `SQLiteStore` + FTS5 | OpenClaw builtin backend（SQLite + FTS5 + sqlite-vec） |
| `applyTimeDecay` 时间衰减 | OpenClaw 的时间衰减算法 |
| `mmrRerank` MMR 去重 | OpenClaw 的 MMR 重排序 |
| `MemoryManager.InjectMemories` | `src/context-engine/` 的 `assemble()` |
| `ExtractAndSave` 异步提取 | OpenClaw 的 `ingest()` |
| `memory.search` RPC 方法 | OpenClaw 的 `memory.*` RPC 方法组 |

---

## 下一阶段预告

Phase 1-8 都只支持 Telegram 一个渠道。
Phase 9 将接入 **Discord**，验证我们的 Channel 抽象层是否真正实现了"开闭原则"：
接入新渠道时，Gateway、Agent、Session、Memory 代码**一行都不改**。
