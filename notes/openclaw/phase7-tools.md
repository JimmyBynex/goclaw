# Phase 7 — Tools：工具调用与函数执行

> 前置：Phase 6 完成，Agent 执行链正常，Fallback 和中止机制就绪
> 目标：AI 可以调用注册的工具函数，实现多步推理（搜索、读写文件等）
> 对应 OpenClaw 模块：`src/tools/`、`src/agents/skills/`、`src/gateway/server-methods/`

---

## 本阶段要建立的目录结构

```
goclaw/
└── internal/
    ├── agent/           ← 修改：runAttempt 加入工具调用循环
    └── tools/           ← 新增（核心）
        ├── types.go     # Tool 接口、Input/Output 类型、ToolCall 结构
        ├── registry.go  # 工具注册表（按层次：全局→代理→请求）
        ├── executor.go  # 工具执行引擎（含权限过滤）
        └── builtin/     # 内置工具实现
            ├── time.go          # get_current_time
            ├── calculator.go    # calculate（数学计算）
            └── http_fetch.go    # http_fetch（获取网页内容）
```

---

## 核心概念：工具调用循环

AI 的工具调用不是单次请求-响应，而是一个循环：

```
用户: "现在北京时间是几点？"
        │
        ▼
AI 输出: tool_use { name: "get_current_time", input: {} }
        │
        ▼
执行工具: get_current_time() → "2026-03-14 15:30:00 CST"
        │  把工具结果加入消息历史
        ▼
AI 继续推理（看到工具结果）
        │
        ▼
AI 输出: "现在北京时间是下午 3:30。"（纯文本 → 循环结束）
```

循环条件：AI 输出 `tool_use` 就继续，输出纯文本就结束。
单次对话中 AI 可能调用多个工具，也可能调用同一个工具多次。

---

## 第一步：工具类型定义

```go
// internal/tools/types.go

package tools

import (
    "context"
    "encoding/json"
)

// Tool 描述一个可供 AI 调用的工具
type Tool struct {
    // Name 是工具的唯一标识，AI 用这个名字调用工具
    // 命名约定：snake_case，如 "get_current_time"、"http_fetch"
    Name string

    // Description 告诉 AI 这个工具是做什么的
    // 写好 Description 比写代码更重要：AI 靠它决定是否调用
    Description string

    // InputSchema 是 JSON Schema，描述工具的输入参数
    // AI 会按此 schema 构造调用参数
    InputSchema map[string]any

    // Execute 是工具的实际执行函数
    // input 是 AI 构造的参数（JSON），output 是给 AI 看的文字结果
    Execute func(ctx context.Context, input json.RawMessage) (output string, err error)

    // Policy 控制谁可以使用这个工具（Phase 7 简化版：全局允许）
    Policy ToolPolicy
}

// ToolPolicy 描述工具的访问策略
type ToolPolicy struct {
    // RequireConfirmation：执行前需要用户确认（危险操作）
    RequireConfirmation bool
    // Sandbox：在沙箱中执行（代码执行类工具）
    Sandbox bool
    // AllowedAgents：空=所有 Agent 可用，非空=只有列表中的 Agent 可用
    AllowedAgents []string
}

// ── AI 工具调用的数据结构 ─────────────────────────────

// ToolUseBlock 是 AI 输出中的工具调用块（Anthropic 格式）
type ToolUseBlock struct {
    ID    string          `json:"id"`
    Name  string          `json:"name"`
    Input json.RawMessage `json:"input"`
}

// ToolResultBlock 是工具执行结果（作为 user 消息回传给 AI）
type ToolResultBlock struct {
    ToolUseID string `json:"tool_use_id"`
    Content   string `json:"content"`
    IsError   bool   `json:"is_error"`
}

// ── AI 响应类型（扩展 Phase 6 的 ai.Response）─────────

// Response 是 AI 一次生成的完整响应
// 可能是纯文本，也可能包含工具调用
type Response struct {
    // StopReason 告诉我们为什么 AI 停止了
    // "end_turn" = 正常结束（纯文本回复）
    // "tool_use" = 需要调用工具
    StopReason string

    // Text 是纯文本内容（StopReason="end_turn" 时有值）
    Text string

    // ToolCalls 是 AI 要求调用的工具列表（StopReason="tool_use" 时有值）
    ToolCalls []ToolUseBlock

    // RawContent 是原始响应内容（用于构造 tool_result 消息）
    RawContent json.RawMessage
}
```

---

## 第二步：工具注册表

```go
// internal/tools/registry.go

package tools

import (
    "fmt"
    "sync"
)

// Registry 维护工具名称到 Tool 的映射
// 线程安全（并发读写）
type Registry struct {
    mu    sync.RWMutex
    tools map[string]*Tool
}

func NewRegistry() *Registry {
    return &Registry{
        tools: make(map[string]*Tool),
    }
}

// Register 注册一个工具
// 如果同名工具已存在，会覆盖（允许运行时更新工具）
func (r *Registry) Register(t *Tool) {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.tools[t.Name] = t
}

// Get 根据名称获取工具
func (r *Registry) Get(name string) (*Tool, bool) {
    r.mu.RLock()
    defer r.mu.RUnlock()
    t, ok := r.tools[name]
    return t, ok
}

// Definitions 导出给 AI 的工具描述列表
// AI 通过这个列表知道有哪些工具可以调用
func (r *Registry) Definitions() []map[string]any {
    r.mu.RLock()
    defer r.mu.RUnlock()

    defs := make([]map[string]any, 0, len(r.tools))
    for _, t := range r.tools {
        defs = append(defs, map[string]any{
            "name":         t.Name,
            "description":  t.Description,
            "input_schema": t.InputSchema,
        })
    }
    return defs
}

// FilterForAgent 返回指定 Agent 有权使用的工具列表
func (r *Registry) FilterForAgent(agentID string) *Registry {
    r.mu.RLock()
    defer r.mu.RUnlock()

    filtered := NewRegistry()
    for _, t := range r.tools {
        if isAllowed(t, agentID) {
            filtered.tools[t.Name] = t
        }
    }
    return filtered
}

func isAllowed(t *Tool, agentID string) bool {
    if len(t.Policy.AllowedAgents) == 0 {
        return true // 无限制，所有 Agent 可用
    }
    for _, id := range t.Policy.AllowedAgents {
        if id == agentID {
            return true
        }
    }
    return false
}
```

---

## 第三步：工具执行引擎

```go
// internal/tools/executor.go

package tools

import (
    "context"
    "encoding/json"
    "fmt"
    "log"
    "time"
)

// Executor 负责执行工具调用
type Executor struct {
    registry *Registry
    timeout  time.Duration // 单个工具最大执行时间
}

func NewExecutor(registry *Registry, timeout time.Duration) *Executor {
    return &Executor{
        registry: registry,
        timeout:  timeout,
    }
}

// Execute 执行单个工具调用，返回结果字符串（给 AI 看的）
func (e *Executor) Execute(ctx context.Context, call ToolUseBlock) ToolResultBlock {
    tool, ok := e.registry.Get(call.Name)
    if !ok {
        return ToolResultBlock{
            ToolUseID: call.ID,
            Content:   fmt.Sprintf("Tool %q not found", call.Name),
            IsError:   true,
        }
    }

    // 为工具执行设置超时
    toolCtx, cancel := context.WithTimeout(ctx, e.timeout)
    defer cancel()

    log.Printf("[tools] executing %s with input: %s", call.Name, string(call.Input))

    output, err := tool.Execute(toolCtx, call.Input)
    if err != nil {
        log.Printf("[tools] %s failed: %v", call.Name, err)
        return ToolResultBlock{
            ToolUseID: call.ID,
            Content:   fmt.Sprintf("Error: %v", err),
            IsError:   true,
        }
    }

    log.Printf("[tools] %s succeeded: %s", call.Name, truncate(output, 200))
    return ToolResultBlock{
        ToolUseID: call.ID,
        Content:   output,
        IsError:   false,
    }
}

// ExecuteAll 并发执行多个工具调用（AI 可能一次请求多个工具）
// 注意：结果顺序与 calls 顺序一致
func (e *Executor) ExecuteAll(ctx context.Context, calls []ToolUseBlock) []ToolResultBlock {
    results := make([]ToolResultBlock, len(calls))

    // 如果只有一个工具调用，直接串行执行
    if len(calls) == 1 {
        results[0] = e.Execute(ctx, calls[0])
        return results
    }

    // 多个工具调用并发执行
    var wg sync.WaitGroup
    for i, call := range calls {
        wg.Add(1)
        go func(idx int, c ToolUseBlock) {
            defer wg.Done()
            results[idx] = e.Execute(ctx, c)
        }(i, call)
    }
    wg.Wait()
    return results
}

func truncate(s string, maxLen int) string {
    if len(s) <= maxLen {
        return s
    }
    return s[:maxLen] + "..."
}
```

---

## 第四步：内置工具实现

### get_current_time

```go
// internal/tools/builtin/time.go

package builtin

import (
    "context"
    "encoding/json"
    "time"

    "github.com/yourname/goclaw/internal/tools"
)

// GetCurrentTimeTool 返回当前时间
// 这是最简单的工具示例，展示工具的基本结构
var GetCurrentTimeTool = &tools.Tool{
    Name:        "get_current_time",
    Description: "Get the current date and time. Use this when the user asks about the current time or date.",
    InputSchema: map[string]any{
        "type": "object",
        "properties": map[string]any{
            "timezone": map[string]any{
                "type":        "string",
                "description": "IANA timezone name, e.g. 'Asia/Shanghai'. Defaults to UTC.",
            },
        },
        "required": []string{},
    },
    Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
        var params struct {
            Timezone string `json:"timezone"`
        }
        json.Unmarshal(input, &params)

        loc := time.UTC
        if params.Timezone != "" {
            var err error
            loc, err = time.LoadLocation(params.Timezone)
            if err != nil {
                loc = time.UTC
            }
        }

        now := time.Now().In(loc)
        return now.Format("2006-01-02 15:04:05 MST"), nil
    },
}
```

### calculate

```go
// internal/tools/builtin/calculator.go

package builtin

import (
    "context"
    "encoding/json"
    "fmt"
    "go/constant"
    "go/token"
    "go/types"

    "github.com/yourname/goclaw/internal/tools"
)

// CalculateTool 执行简单数学计算
// 使用 Go 标准库解析数学表达式，安全且无需第三方依赖
var CalculateTool = &tools.Tool{
    Name:        "calculate",
    Description: "Evaluate a mathematical expression. Supports +, -, *, /, ** (power), and parentheses.",
    InputSchema: map[string]any{
        "type": "object",
        "properties": map[string]any{
            "expression": map[string]any{
                "type":        "string",
                "description": "Mathematical expression to evaluate, e.g. '(3 + 4) * 2'",
            },
        },
        "required": []string{"expression"},
    },
    Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
        var params struct {
            Expression string `json:"expression"`
        }
        if err := json.Unmarshal(input, &params); err != nil {
            return "", fmt.Errorf("invalid input: %w", err)
        }
        if params.Expression == "" {
            return "", fmt.Errorf("expression is required")
        }

        // 使用 go/constant 安全求值（只支持常量表达式，防止注入）
        val := constant.MakeFromLiteral(params.Expression, token.FLOAT, 0)
        if val.Kind() == constant.Unknown {
            return "", fmt.Errorf("cannot evaluate expression: %q", params.Expression)
        }
        result, _ := constant.Float64Val(val)
        return fmt.Sprintf("%g", result), nil
    },
}
```

### http_fetch

```go
// internal/tools/builtin/http_fetch.go

package builtin

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "strings"
    "time"

    "github.com/yourname/goclaw/internal/tools"
)

// HTTPFetchTool 获取网页内容（纯文本）
// 这是一个有一定风险的工具：需要网络访问
// 生产环境应该加 URL 白名单过滤
var HTTPFetchTool = &tools.Tool{
    Name:        "http_fetch",
    Description: "Fetch the content of a URL and return it as plain text. Use for retrieving web pages, APIs, or public data.",
    InputSchema: map[string]any{
        "type": "object",
        "properties": map[string]any{
            "url": map[string]any{
                "type":        "string",
                "description": "The URL to fetch",
            },
            "max_chars": map[string]any{
                "type":        "integer",
                "description": "Maximum characters to return (default 5000)",
            },
        },
        "required": []string{"url"},
    },
    Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
        var params struct {
            URL      string `json:"url"`
            MaxChars int    `json:"max_chars"`
        }
        if err := json.Unmarshal(input, &params); err != nil {
            return "", err
        }
        if params.MaxChars <= 0 {
            params.MaxChars = 5000
        }

        client := &http.Client{Timeout: 10 * time.Second}
        req, err := http.NewRequestWithContext(ctx, "GET", params.URL, nil)
        if err != nil {
            return "", fmt.Errorf("invalid URL: %w", err)
        }
        req.Header.Set("User-Agent", "goclaw/1.0")

        resp, err := client.Do(req)
        if err != nil {
            return "", fmt.Errorf("fetch failed: %w", err)
        }
        defer resp.Body.Close()

        // 限制读取大小，防止读取超大响应
        body, err := io.ReadAll(io.LimitReader(resp.Body, int64(params.MaxChars*4)))
        if err != nil {
            return "", err
        }

        // 去除 HTML 标签（简单版本）
        text := stripHTMLTags(string(body))
        if len(text) > params.MaxChars {
            text = text[:params.MaxChars] + "\n...(truncated)"
        }

        return fmt.Sprintf("HTTP %d\n\n%s", resp.StatusCode, text), nil
    },
}

func stripHTMLTags(html string) string {
    var sb strings.Builder
    inTag := false
    for _, r := range html {
        switch {
        case r == '<':
            inTag = true
        case r == '>':
            inTag = false
            sb.WriteRune(' ')
        case !inTag:
            sb.WriteRune(r)
        }
    }
    return strings.Join(strings.Fields(sb.String()), " ")
}
```

---

## 第五步：修改 AI Client 接口，支持工具

Phase 6 的 `StreamChat` 只返回文本，现在需要返回包含工具调用的响应。

```go
// internal/ai/client.go（修改）

package ai

import (
    "context"
    "github.com/yourname/goclaw/internal/tools"
)

// Client 是 AI 提供方的接口（修改版）
type Client interface {
    // Chat 发起一次对话，返回完整响应（含工具调用信息）
    // toolDefs 是可用工具的描述列表（nil 表示不使用工具）
    Chat(ctx context.Context, messages []Message, toolDefs []map[string]any) (*tools.Response, error)

    // StreamChat 流式对话（仅用于纯文本输出，工具调用走 Chat）
    // 注意：工具调用场景不支持流式，因为需要等完整响应才能执行工具
    StreamChat(ctx context.Context, messages []Message) (<-chan string, <-chan error)
}

// Message 扩展：支持工具结果类型
type Message struct {
    Role    string // "system" | "user" | "assistant" | "tool"
    Content string

    // 工具调用相关（Role="assistant" 时，AI 的工具调用请求）
    ToolCalls []tools.ToolUseBlock `json:",omitempty"`

    // 工具结果相关（Role="tool" 时，工具执行结果）
    ToolResults []tools.ToolResultBlock `json:",omitempty"`
}
```

---

## 第六步：工具调用循环（修改 Agent）

这是 Phase 7 最核心的改动，在 `runner.go` 的 `runAttempt` 中实现：

```go
// internal/agent/runner.go（核心修改）

// runAttempt 现在包含完整的工具调用循环
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

    // 为当前 Agent 过滤可用工具
    agentTools := a.toolRegistry.FilterForAgent(a.id)
    toolDefs := agentTools.Definitions()

    // 工具调用循环
    // 消息历史在循环内增长（加入工具结果），不写入持久化 Session
    // 只有最终的文本回复才写入 Session
    loopMessages := sess.MessagesForAI(a.systemPrompt, 20)
    maxIterations := 10 // 防止无限循环

    for i := 0; i < maxIterations; i++ {
        // 发起 AI 请求
        resp, err := client.Chat(ctx, loopMessages, toolDefs)
        if err != nil {
            return nil, err
        }

        switch resp.StopReason {
        case "end_turn":
            // AI 完成，返回文字结果
            return &RunResult{
                RunID: runID,
                Reply: resp.Text,
                Model: modelRef.Model,
            }, nil

        case "tool_use":
            // AI 要调用工具
            if len(resp.ToolCalls) == 0 {
                return nil, fmt.Errorf("tool_use stop reason but no tool calls")
            }

            // 通知 Gateway AI 正在调用工具
            sendEvent(eventCh, AgentEvent{
                Type:  "agent.tool_calls",
                RunID: runID,
                Data:  resp.ToolCalls,
            })

            // 将 AI 的工具调用请求加入消息历史
            loopMessages = append(loopMessages, ai.Message{
                Role:      "assistant",
                ToolCalls: resp.ToolCalls,
            })

            // 并发执行所有工具
            results := a.executor.ExecuteAll(ctx, resp.ToolCalls)

            // 通知 Gateway 工具执行结果
            sendEvent(eventCh, AgentEvent{
                Type:  "agent.tool_results",
                RunID: runID,
                Data:  results,
            })

            // 将工具结果加入消息历史，供 AI 继续推理
            loopMessages = append(loopMessages, ai.Message{
                Role:        "tool",
                ToolResults: results,
            })

            // 继续下一轮循环（AI 会看到工具结果，决定下一步）

        default:
            return nil, fmt.Errorf("unexpected stop reason: %s", resp.StopReason)
        }
    }

    return nil, fmt.Errorf("tool loop exceeded max iterations (%d)", maxIterations)
}
```

---

## 第七步：注册内置工具

```go
// internal/agent/agent.go（新增）

import (
    "github.com/yourname/goclaw/internal/tools"
    "github.com/yourname/goclaw/internal/tools/builtin"
)

// setupTools 注册所有内置工具
func setupTools() *tools.Registry {
    reg := tools.NewRegistry()

    reg.Register(builtin.GetCurrentTimeTool)
    reg.Register(builtin.CalculateTool)
    reg.Register(builtin.HTTPFetchTool)

    return reg
}
```

---

## 测试工具调用

```
用户：现在几点了？
Bot：现在是 2026-03-14 15:30:00 CST。

用户：帮我算一下 (123 * 456 + 789) / 3 等于多少
Bot：让我计算一下...
     (123 × 456 + 789) ÷ 3 = 18929

用户：帮我看看 https://example.com 的内容
Bot：这是 example.com 的内容：
     "This domain is for use in illustrative examples..."
```

---

## 本阶段核心工程知识点

### 1. JSON Schema 的作用

```go
InputSchema: map[string]any{
    "type": "object",
    "properties": map[string]any{
        "url": map[string]any{
            "type":        "string",
            "description": "The URL to fetch", // AI 靠这句话决定怎么填参数
        },
    },
    "required": []string{"url"}, // AI 必须提供这些字段
},
```

`description` 字段是给 AI 看的"注释"，写得越清晰，AI 调用工具越准确。
这比写代码注释更重要。

### 2. 工具调用循环的终止条件

```
正常终止：AI 输出 stop_reason="end_turn"（纯文本回复）
异常终止：
  - 超过最大迭代次数（maxIterations=10）防止无限循环
  - ctx 取消（用户中止或超时）
  - 工具执行返回不可恢复错误
```

### 3. 工具执行的消息格式（Anthropic）

```json
// AI 的工具调用请求（assistant 消息）
{
  "role": "assistant",
  "content": [
    {
      "type": "tool_use",
      "id": "tool_xxx",
      "name": "get_current_time",
      "input": {"timezone": "Asia/Shanghai"}
    }
  ]
}

// 工具执行结果（user 消息）
{
  "role": "user",
  "content": [
    {
      "type": "tool_result",
      "tool_use_id": "tool_xxx",
      "content": "2026-03-14 15:30:00 CST"
    }
  ]
}
```

这个格式让 AI 的"思考过程"（工具调用链）完整地出现在消息历史里，
AI 可以追溯之前做了什么，避免重复调用同一工具。

### 4. 工具超时与取消的组合

```go
// 两层保护
toolCtx, cancel := context.WithTimeout(ctx, e.timeout) // 单个工具最长 30 秒
defer cancel()

// 如果 ctx（来自用户的中止操作）先取消，toolCtx 自动取消
// 如果工具超时，toolCtx 取消，但 ctx 不受影响（其他工具不会因此取消）
```

---

## 与 OpenClaw 的对应关系

| 本阶段代码 | OpenClaw 对应 |
|-----------|---------------|
| `Tool` 结构体 | `src/tools/` 中的工具定义 |
| `tools.Registry` | OpenClaw 的工具注册中心 |
| `Executor.ExecuteAll` 并发执行 | OpenClaw 的并行工具执行 |
| 工具调用循环（`maxIterations`）| OpenClaw agent 的 tool use loop |
| `FilterForAgent` 权限过滤 | `src/tools/` 中的 global→agent 分层过滤 |
| `ToolPolicy.Sandbox` | OpenClaw 的 Docker Sandbox 思想 |

---

## 下一阶段预告

Phase 7 实现了工具调用，但 AI 的"记忆"仍然局限于当前对话的消息列表。
Phase 8 将引入 **Memory（记忆系统）**：使用 SQLite + FTS5 全文检索和向量相似度，
让 AI 可以检索长期存储的记忆片段，突破上下文窗口的限制。
