# Phase 9 — Cron + 结构化数据：提醒、日程、记账

> 前置：Phase 8 完成，记忆系统正常，SQLite 已初始化
> 目标：AI 可以创建提醒/日程/账单，Cron 调度器到点主动推送消息
> 对应 OpenClaw 模块：`src/gateway/server-cron.ts`、`cronHandlers`

---

## 本阶段要建立的目录结构

```
goclaw/
└── internal/
    ├── cron/                  ← 新增（核心）
    │   ├── types.go           # Job 类型定义
    │   ├── store.go           # SQLite 存储 Job
    │   └── scheduler.go       # 后台调度器，到点触发
    ├── structured/            ← 新增（结构化数据）
    │   ├── events.go          # 日程表（CREATE TABLE events）
    │   └── ledger.go          # 记账（CREATE TABLE transactions）
    ├── tools/builtin/         ← 新增工具
    │   ├── reminder.go        # create_reminder / list_reminders / delete_reminder
    │   ├── calendar.go        # create_event / list_events / delete_event
    │   └── ledger.go          # add_transaction / monthly_summary
    ├── gateway/               ← 修改：暴露主动发送接口
    │   └── server.go          # 新增 ActiveSend 方法
    └── agent/                 ← 修改：注册新工具
        └── agent.go           # setupTools 增加新工具
```

---

## 核心问题：主动发送路径

现有消息流是被动的：
```
用户发消息 → 渠道接收 → Gateway → Agent → 回复用户
```

Cron 需要主动推送：
```
时间到了 → Scheduler → Gateway.ActiveSend → ChannelManager → 用户
```

Gateway 需要新增一个主动发送的方法，Scheduler 持有 Gateway 引用。

---

## 第一步：Cron 类型定义

```go
// internal/cron/types.go

package cron

import "time"

// Job 是一条定时任务
type Job struct {
    ID        int64
    AgentID   string    // 属于哪个 Agent
    ChannelID string    // 发往哪个渠道（"telegram"）
    AccountID string    // 渠道账号 ID
    PeerID    string    // 发给谁（用户/群组 ID）
    Message   string    // 到点发送的消息内容
    Schedule  string    // cron 表达式（"0 8 * * *" = 每天8点）或 RFC3339 一次性时间
    Repeat    bool      // false=执行一次后删除，true=按 schedule 重复
    NextRunAt time.Time // 下次执行时间（调度器填写）
    CreatedAt time.Time
    Done      bool      // 一次性任务完成后标记
}
```

---

## 第二步：Cron Store

```go
// internal/cron/store.go

package cron

import (
    "database/sql"
    "time"
)

type Store struct {
    db *sql.DB
}

func NewStore(db *sql.DB) (*Store, error) {
    s := &Store{db: db}
    return s, s.migrate()
}

func (s *Store) migrate() error {
    _, err := s.db.Exec(`
        CREATE TABLE IF NOT EXISTS cron_jobs (
            id          INTEGER PRIMARY KEY AUTOINCREMENT,
            agent_id    TEXT NOT NULL,
            channel_id  TEXT NOT NULL,
            account_id  TEXT NOT NULL,
            peer_id     TEXT NOT NULL,
            message     TEXT NOT NULL,
            schedule    TEXT NOT NULL,
            repeat      BOOLEAN NOT NULL DEFAULT 0,
            next_run_at DATETIME NOT NULL,
            created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
            done        BOOLEAN NOT NULL DEFAULT 0
        );
        CREATE INDEX IF NOT EXISTS idx_cron_next ON cron_jobs(next_run_at, done);
    `)
    return err
}

// Save 保存一个新 Job，填充 ID 和 CreatedAt
func (s *Store) Save(j *Job) error {
    now := time.Now()
    result, err := s.db.Exec(`
        INSERT INTO cron_jobs (agent_id, channel_id, account_id, peer_id, message, schedule, repeat, next_run_at, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
    `, j.AgentID, j.ChannelID, j.AccountID, j.PeerID, j.Message, j.Schedule, j.Repeat, j.NextRunAt, now)
    if err != nil {
        return err
    }
    j.ID, _ = result.LastInsertId()
    j.CreatedAt = now
    return nil
}

// Due 返回所有到期未完成的任务
func (s *Store) Due(now time.Time) ([]*Job, error) {
    rows, err := s.db.Query(`
        SELECT id, agent_id, channel_id, account_id, peer_id, message, schedule, repeat, next_run_at, created_at
        FROM cron_jobs
        WHERE next_run_at <= ? AND done = 0
    `, now)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var jobs []*Job
    for rows.Next() {
        j := &Job{}
        rows.Scan(&j.ID, &j.AgentID, &j.ChannelID, &j.AccountID, &j.PeerID,
            &j.Message, &j.Schedule, &j.Repeat, &j.NextRunAt, &j.CreatedAt)
        jobs = append(jobs, j)
    }
    return jobs, nil
}

// MarkDone 标记一次性任务完成
func (s *Store) MarkDone(id int64) error {
    _, err := s.db.Exec("UPDATE cron_jobs SET done = 1 WHERE id = ?", id)
    return err
}

// UpdateNextRun 更新重复任务的下次运行时间
func (s *Store) UpdateNextRun(id int64, next time.Time) error {
    _, err := s.db.Exec("UPDATE cron_jobs SET next_run_at = ? WHERE id = ?", next, id)
    return err
}

// Delete 删除任务
func (s *Store) Delete(id int64) error {
    _, err := s.db.Exec("DELETE FROM cron_jobs WHERE id = ?", id)
    return err
}

// List 列出某个 Agent 的所有未完成任务
func (s *Store) List(agentID string) ([]*Job, error) {
    rows, err := s.db.Query(`
        SELECT id, agent_id, channel_id, account_id, peer_id, message, schedule, repeat, next_run_at, created_at
        FROM cron_jobs WHERE agent_id = ? AND done = 0
        ORDER BY next_run_at ASC
    `, agentID)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var jobs []*Job
    for rows.Next() {
        j := &Job{}
        rows.Scan(&j.ID, &j.AgentID, &j.ChannelID, &j.AccountID, &j.PeerID,
            &j.Message, &j.Schedule, &j.Repeat, &j.NextRunAt, &j.CreatedAt)
        jobs = append(jobs, j)
    }
    return jobs, nil
}
```

---

## 第三步：Scheduler

```go
// internal/cron/scheduler.go

package cron

import (
    "context"
    "log"
    "time"
)

// Sender 是 Scheduler 依赖的发送接口
// Gateway 实现这个接口，避免循环依赖
type Sender interface {
    ActiveSend(ctx context.Context, channelID, accountID, peerID, text string) error
}

type Scheduler struct {
    store  *Store
    sender Sender
}

func NewScheduler(store *Store, sender Sender) *Scheduler {
    return &Scheduler{store: store, sender: sender}
}

// Start 启动后台调度循环，每分钟检查一次到期任务
func (s *Scheduler) Start(ctx context.Context) {
    go func() {
        ticker := time.NewTicker(time.Minute)
        defer ticker.Stop()

        // 启动时立刻检查一次（防止重启后漏掉到期任务）
        s.tick(ctx)

        for {
            select {
            case <-ticker.C:
                s.tick(ctx)
            case <-ctx.Done():
                return
            }
        }
    }()
}

func (s *Scheduler) tick(ctx context.Context) {
    jobs, err := s.store.Due(time.Now())
    if err != nil {
        log.Printf("[cron] query due jobs error: %v", err)
        return
    }

    for _, j := range jobs {
        go s.execute(ctx, j)
    }
}

func (s *Scheduler) execute(ctx context.Context, j *Job) {
    log.Printf("[cron] executing job id=%d message=%q", j.ID, j.Message)

    err := s.sender.ActiveSend(ctx, j.ChannelID, j.AccountID, j.PeerID, j.Message)
    if err != nil {
        log.Printf("[cron] send failed job id=%d: %v", j.ID, err)
        return
    }

    if j.Repeat {
        // 计算下次运行时间（解析 cron 表达式）
        next, err := nextCronTime(j.Schedule, time.Now())
        if err != nil {
            log.Printf("[cron] parse schedule error: %v", err)
            return
        }
        s.store.UpdateNextRun(j.ID, next)
    } else {
        s.store.MarkDone(j.ID)
    }
}

// nextCronTime 解析 cron 表达式，返回下次执行时间
// 使用 github.com/robfig/cron/v3 库
func nextCronTime(schedule string, from time.Time) (time.Time, error) {
    // TODO: 引入 robfig/cron 解析
    // parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
    // sched, err := parser.Parse(schedule)
    // return sched.Next(from), err
    return from.Add(24 * time.Hour), nil // 占位实现
}
```

---

## 第四步：Gateway 新增主动发送接口

修改 `internal/gateway/server.go`，实现 `Sender` 接口：

```go
// internal/gateway/server.go（新增方法）

// ActiveSend 主动向指定渠道用户发送消息（供 Cron 调度器调用）
func (g *Gateway) ActiveSend(ctx context.Context, channelID, accountID, peerID, text string) error {
    ch, err := g.channelMgr.Get(channelID, accountID)
    if err != nil {
        return fmt.Errorf("active send: channel not found %s/%s: %w", channelID, accountID, err)
    }
    _, err = ch.Send(ctx, channel.OutboundMessage{
        PeerID: peerID,
        Text:   text,
    })
    return err
}
```

---

## 第五步：结构化数据（日程 + 记账）

```go
// internal/structured/events.go

package structured

import (
    "database/sql"
    "time"
)

type Event struct {
    ID        int64
    AgentID   string
    Title     string
    StartAt   time.Time
    EndAt     time.Time
    Location  string
    Note      string
    CreatedAt time.Time
}

type EventStore struct{ db *sql.DB }

func NewEventStore(db *sql.DB) (*EventStore, error) {
    s := &EventStore{db: db}
    return s, s.migrate()
}

func (s *EventStore) migrate() error {
    _, err := s.db.Exec(`
        CREATE TABLE IF NOT EXISTS events (
            id         INTEGER PRIMARY KEY AUTOINCREMENT,
            agent_id   TEXT NOT NULL,
            title      TEXT NOT NULL,
            start_at   DATETIME NOT NULL,
            end_at     DATETIME,
            location   TEXT,
            note       TEXT,
            created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
        );
        CREATE INDEX IF NOT EXISTS idx_events_agent_start ON events(agent_id, start_at);
    `)
    return err
}

func (s *EventStore) Save(e *Event) error {
    result, err := s.db.Exec(`
        INSERT INTO events (agent_id, title, start_at, end_at, location, note, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?)
    `, e.AgentID, e.Title, e.StartAt, e.EndAt, e.Location, e.Note, time.Now())
    if err != nil {
        return err
    }
    e.ID, _ = result.LastInsertId()
    return nil
}

// ListByRange 查询时间范围内的日程
func (s *EventStore) ListByRange(agentID string, from, to time.Time) ([]*Event, error) {
    rows, err := s.db.Query(`
        SELECT id, agent_id, title, start_at, end_at, location, note, created_at
        FROM events
        WHERE agent_id = ? AND start_at >= ? AND start_at <= ?
        ORDER BY start_at ASC
    `, agentID, from, to)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var events []*Event
    for rows.Next() {
        e := &Event{}
        rows.Scan(&e.ID, &e.AgentID, &e.Title, &e.StartAt, &e.EndAt,
            &e.Location, &e.Note, &e.CreatedAt)
        events = append(events, e)
    }
    return events, nil
}

func (s *EventStore) Delete(id int64) error {
    _, err := s.db.Exec("DELETE FROM events WHERE id = ?", id)
    return err
}
```

```go
// internal/structured/ledger.go

package structured

import (
    "database/sql"
    "time"
)

type Transaction struct {
    ID          int64
    AgentID     string
    Amount      float64   // 正数=收入，负数=支出
    Category    string    // "餐饮" | "交通" | "学习" | "工资" 等
    Note        string
    HappenedAt  time.Time
    CreatedAt   time.Time
}

type MonthlySummary struct {
    Month    string             // "2026-03"
    Total    float64            // 净额
    ByCategory map[string]float64
}

type LedgerStore struct{ db *sql.DB }

func NewLedgerStore(db *sql.DB) (*LedgerStore, error) {
    s := &LedgerStore{db: db}
    return s, s.migrate()
}

func (s *LedgerStore) migrate() error {
    _, err := s.db.Exec(`
        CREATE TABLE IF NOT EXISTS transactions (
            id           INTEGER PRIMARY KEY AUTOINCREMENT,
            agent_id     TEXT NOT NULL,
            amount       REAL NOT NULL,
            category     TEXT NOT NULL DEFAULT 'other',
            note         TEXT,
            happened_at  DATETIME NOT NULL,
            created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
        );
        CREATE INDEX IF NOT EXISTS idx_tx_agent_time ON transactions(agent_id, happened_at);
    `)
    return err
}

func (s *LedgerStore) Save(t *Transaction) error {
    result, err := s.db.Exec(`
        INSERT INTO transactions (agent_id, amount, category, note, happened_at, created_at)
        VALUES (?, ?, ?, ?, ?, ?)
    `, t.AgentID, t.Amount, t.Category, t.Note, t.HappenedAt, time.Now())
    if err != nil {
        return err
    }
    t.ID, _ = result.LastInsertId()
    return nil
}

// MonthlySummary 返回指定月份的收支汇总（"2026-03"）
func (s *LedgerStore) MonthlySummary(agentID, month string) (*MonthlySummary, error) {
    rows, err := s.db.Query(`
        SELECT category, SUM(amount)
        FROM transactions
        WHERE agent_id = ? AND strftime('%Y-%m', happened_at) = ?
        GROUP BY category
    `, agentID, month)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    summary := &MonthlySummary{
        Month:      month,
        ByCategory: make(map[string]float64),
    }
    for rows.Next() {
        var cat string
        var sum float64
        rows.Scan(&cat, &sum)
        summary.ByCategory[cat] = sum
        summary.Total += sum
    }
    return summary, nil
}
```

---

## 第六步：AI 工具

```go
// internal/tools/builtin/reminder.go

package builtin

import (
    "context"
    "encoding/json"
    "fmt"
    "time"

    "goclaw/internal/cron"
    "goclaw/internal/tools"
)

// RegisterReminderTools 注册提醒相关工具
func RegisterReminderTools(registry *tools.Registry, store *cron.Store, agentID, channelID, accountID, peerID string) {
    registry.Register(tools.Tool{
        Name:        "create_reminder",
        Description: "创建一个定时提醒。时间格式：RFC3339（如 2026-03-29T09:00:00+08:00）或 cron 表达式（如 0 8 * * * 表示每天8点）",
        InputSchema: map[string]any{
            "type": "object",
            "properties": map[string]any{
                "message":  map[string]any{"type": "string", "description": "提醒内容"},
                "schedule": map[string]any{"type": "string", "description": "触发时间或 cron 表达式"},
                "repeat":   map[string]any{"type": "boolean", "description": "是否重复，true=按 cron 重复，false=只触发一次"},
            },
            "required": []string{"message", "schedule"},
        },
        Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
            var p struct {
                Message  string `json:"message"`
                Schedule string `json:"schedule"`
                Repeat   bool   `json:"repeat"`
            }
            if err := json.Unmarshal(input, &p); err != nil {
                return "", err
            }

            nextRun, err := parseSchedule(p.Schedule)
            if err != nil {
                return "", fmt.Errorf("无法解析时间: %w", err)
            }

            job := &cron.Job{
                AgentID:   agentID,
                ChannelID: channelID,
                AccountID: accountID,
                PeerID:    peerID,
                Message:   p.Message,
                Schedule:  p.Schedule,
                Repeat:    p.Repeat,
                NextRunAt: nextRun,
            }
            if err := store.Save(job); err != nil {
                return "", err
            }
            return fmt.Sprintf("✅ 提醒已创建，ID=%d，将于 %s 触发", job.ID, nextRun.Format("2006-01-02 15:04")), nil
        },
    })

    registry.Register(tools.Tool{
        Name:        "list_reminders",
        Description: "列出所有待触发的提醒",
        InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
        Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
            jobs, err := store.List(agentID)
            if err != nil {
                return "", err
            }
            if len(jobs) == 0 {
                return "暂无提醒", nil
            }
            var result string
            for _, j := range jobs {
                result += fmt.Sprintf("ID=%d | %s | 下次: %s\n",
                    j.ID, j.Message, j.NextRunAt.Format("2006-01-02 15:04"))
            }
            return result, nil
        },
    })

    registry.Register(tools.Tool{
        Name:        "delete_reminder",
        Description: "删除一个提醒",
        InputSchema: map[string]any{
            "type": "object",
            "properties": map[string]any{
                "id": map[string]any{"type": "integer", "description": "提醒 ID"},
            },
            "required": []string{"id"},
        },
        Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
            var p struct{ ID int64 `json:"id"` }
            json.Unmarshal(input, &p)
            if err := store.Delete(p.ID); err != nil {
                return "", err
            }
            return fmt.Sprintf("✅ 提醒 ID=%d 已删除", p.ID), nil
        },
    })
}

// parseSchedule 解析时间字符串（RFC3339 或简单自然语言）
func parseSchedule(s string) (time.Time, error) {
    // 尝试 RFC3339
    if t, err := time.Parse(time.RFC3339, s); err == nil {
        return t, nil
    }
    // 尝试常见格式
    formats := []string{
        "2006-01-02 15:04",
        "2006-01-02T15:04",
        "01-02 15:04",
    }
    for _, f := range formats {
        if t, err := time.ParseInLocation(f, s, time.Local); err == nil {
            // 如果没有年份，补上当前年
            if t.Year() == 0 {
                t = t.AddDate(time.Now().Year(), 0, 0)
            }
            return t, nil
        }
    }
    return time.Time{}, fmt.Errorf("unsupported time format: %s", s)
}
```

---

## 第七步：集成到 Agent

Agent 初始化时注册工具，需要把当前对话的渠道信息传进去：

```go
// internal/agent/agent.go（修改 setupTools，新增结构化工具）

// 问题：工具创建提醒时需要知道往哪个渠道发消息
// 解法：提醒工具在 RunReply 调用时动态注册（而不是 Agent 初始化时）
// 因为 channelID/peerID 来自入站消息，初始化时不知道

// RunReply 修改：传入消息的渠道信息，动态注册提醒工具
func (a *Agent) RunReply(
    parentCtx context.Context,
    sess *session.Session,
    userText string,
    runID string,
    eventCh chan<- AgentEvent,
    // 新增：
    channelID, accountID, peerID string,
) (*RunResult, error) {
    // ... 原有代码 ...

    // 动态注册提醒工具（绑定当前对话的渠道信息）
    sessionRegistry := a.toolRegistry.Clone() // 复制一份，避免并发冲突
    builtin.RegisterReminderTools(sessionRegistry, a.cronStore, a.id, channelID, accountID, peerID)
    builtin.RegisterCalendarTools(sessionRegistry, a.eventStore, a.id)
    builtin.RegisterLedgerTools(sessionRegistry, a.ledgerStore, a.id)

    // ... 后续使用 sessionRegistry 而不是 a.toolRegistry ...
}
```

---

## 第八步：main.go 集成

```go
// main.go 关键变更

// 共享同一个 SQLite db 实例
db, err := sql.Open("sqlite", "file:goclaw.db?_journal_mode=WAL")

// 初始化各 Store
cronStore, _    := cron.NewStore(db)
eventStore, _   := structured.NewEventStore(db)
ledgerStore, _  := structured.NewLedgerStore(db)

// 初始化 Scheduler
scheduler := cron.NewScheduler(cronStore, gw)  // gw 实现 Sender 接口
scheduler.Start(ctx)

// Agent 注入新 Store
// （需要在 agent.FromConfig 或 AgentRegistry 里传入）
```

---

## 测试场景

```
# 提醒
用户: 明天早上9点提醒我交作业
Bot: ✅ 提醒已创建，ID=1，将于 2026-03-29 09:00 触发
（第二天9点）Bot: 提醒你：交作业

# 日程
用户: 帮我记录下周一下午3点有高数考试，地点在A栋101
Bot: ✅ 已添加日程：高数考试，2026-03-30 15:00，A栋101
用户: 这周有什么安排
Bot: 本周日程：
     1. 2026-03-30 15:00 高数考试（A栋101）

# 记账
用户: 今天午饭花了35块
Bot: ✅ 已记录：-35.00 元，分类：餐饮
用户: 这个月花了多少
Bot: 2026-03 消费汇总：
     餐饮：-320 元
     交通：-85 元
     合计：-405 元
```

---

## 本阶段核心工程知识点

### 1. 动态工具注册 vs 静态工具注册

提醒工具需要知道往哪里发消息，这个信息在对话开始时才有。
解法是在 `RunReply` 调用时克隆一份 Registry，动态注册带渠道信息的工具：

```
全局 toolRegistry    ← Agent 初始化时注册（get_time, calculate 等无状态工具）
sessionRegistry      ← RunReply 时克隆 + 动态注册（create_reminder 等有状态工具）
```

### 2. Scheduler 依赖接口而非具体类型

```go
type Sender interface {
    ActiveSend(ctx context.Context, channelID, accountID, peerID, text string) error
}
```

Scheduler 不直接依赖 Gateway，依赖接口。好处：
- 避免循环依赖（cron 包不 import gateway 包）
- 测试时可以 mock

### 3. 共享 SQLite 连接

cron、structured、memory 三个包共用同一个 `*sql.DB`：

```
main.go 打开一次 db
  → cronStore(db)
  → eventStore(db)
  → ledgerStore(db)
  → memoryStore(db)  ← Phase 8 的
```

每个 Store 各自 migrate（CREATE TABLE IF NOT EXISTS），互不干扰。
SQLite WAL 模式保证并发写入安全。

### 4. 时间处理

Go 的时间处理要注意时区：

```go
// 存入 SQLite 时用 UTC
time.Now().UTC()

// 解析用户输入的时间要指定本地时区
time.ParseInLocation("2006-01-02 15:04", s, time.Local)
```

用户说"明天9点"，AI 应该转成完整 RFC3339 格式再传给工具，避免歧义。

---

## 与 OpenClaw 的对应关系

| 本阶段代码 | OpenClaw 对应 |
|-----------|---------------|
| `cron.Store` + `cron.Scheduler` | `src/gateway/server-cron.ts` |
| `cronHandlers`（RPC）| `cronHandlers`（`cron.add / cron.run / cron.delete`）|
| `Gateway.ActiveSend` | OpenClaw 渠道的主动发送 adapter |
| `structured.EventStore` | 可用 memory 工具 + 结构化提取替代 |
| 动态工具注册 | OpenClaw 的 skill 按 session 隔离 |

---

## 下一阶段

原 Phase 9（Discord 多渠道）后移为 Phase 10：
接入 Discord，验证 Channel 抽象层是否真正实现开闭原则——
Gateway、Agent、Session、Memory、Cron 代码一行都不改。
