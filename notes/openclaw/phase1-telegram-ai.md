# Phase 1 — 最小可用版本：Telegram + AI 流式回复

> 前置：无
> 目标：Telegram 发消息 → 通过 OpenRouter 调用 Claude → 流式回复到 Telegram
> 对应 OpenClaw 模块：`src/channels/telegram/`、`src/agents/pi-embedded-runner/`

---

## 本阶段要建立的目录结构

```
goclaw/
├── go.mod
├── go.sum
├── main.go
├── config.yaml
└── internal/
    ├── telegram/
    │   ├── bot.go       # Bot 生命周期、消息循环
    │   ├── types.go     # Telegram API 结构体（Update、Message 等）
    │   └── send.go      # 发送/编辑消息方法
    └── ai/
        ├── client.go    # Client 接口定义
        ├── message.go   # 通用 Message 类型
        └── openrouter/
            └── openrouter.go  # OpenRouter 实现（OpenAI 兼容格式）
```

---

## 第一步：初始化项目

```bash
mkdir goclaw && cd goclaw
go mod init github.com/yourname/goclaw

# 暂时只需要标准库，后续按需 go get
```

---

## 第二步：配置文件

最简配置，先硬编码结构，Phase 4 再做热重载。

```yaml
# config.yaml
telegram:
  token: "YOUR_BOT_TOKEN"    # 从 @BotFather 获取

ai:
  api_key: "sk-or-..."       # 从 https://openrouter.ai/keys 获取
  model: "anthropic/claude-sonnet-4-6"   # OpenRouter 的模型名格式：provider/model-name
  system_prompt: "You are a helpful assistant."
```

---

## 第三步：AI 客户端接口

先定义接口，再写实现。接口设计要足够小，只表达"流式对话"这一个能力。

```go
// internal/ai/message.go

package ai

// Message 是与平台无关的对话消息单元
// Role 只能是 "system" | "user" | "assistant"
type Message struct {
    Role    string
    Content string
}
```

```go
// internal/ai/client.go

package ai

import "context"

// Client 是所有 AI 提供方必须实现的接口
// 只有一个方法：流式聊天
// 通过 channel 返回文本块，调用方负责消费和关闭判断
type Client interface {
    // StreamChat 发起一次对话请求
    // 返回的 channel 会持续输出文本块，关闭后表示完成
    // ctx 取消时，实现方需要停止输出并关闭 channel
    StreamChat(ctx context.Context, messages []Message) (<-chan string, <-chan error)
}
```

> **为什么返回两个 channel？**
> 一个输出文本块，一个输出错误。调用方用 `select` 同时监听两者。
> 如果只返回 `(<-chan string, error)`，就变成阻塞调用，失去流式意义。

---

## 第四步：OpenRouter 实现

OpenRouter 提供 **OpenAI 兼容的 API**，SSE 流格式与 OpenAI 相同，比 Anthropic 原生格式更简单。
好处：同一套代码未来可以无缝切换到任何 OpenRouter 支持的模型（GPT、Gemini、Llama 等）。

```go
// internal/ai/openrouter/openrouter.go

package openrouter

import (
    "bufio"
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "strings"
    "time"

    "github.com/yourname/goclaw/internal/ai"
)

type Client struct {
    apiKey string
    model  string
    http   *http.Client
}

// New 创建 OpenRouter 客户端
// apiKey：从 https://openrouter.ai/keys 获取
// model：如 "anthropic/claude-sonnet-4-6"，完整列表见 https://openrouter.ai/models
func New(apiKey, model string) *Client {
    return &Client{
        apiKey: apiKey,
        model:  model,
        http:   &http.Client{Timeout: 120 * time.Second},
    }
}

// OpenAI 兼容的请求结构
type chatRequest struct {
    Model    string       `json:"model"`
    Messages []msgPayload `json:"messages"`
    Stream   bool         `json:"stream"`
}

type msgPayload struct {
    Role    string `json:"role"`
    Content string `json:"content"`
}

// SSE 流中每个 data: 行的结构（OpenAI 格式）
type streamChunk struct {
    Choices []struct {
        Delta struct {
            Content string `json:"content"`
        } `json:"delta"`
    } `json:"choices"`
}

func (c *Client) StreamChat(ctx context.Context, messages []ai.Message) (<-chan string, <-chan error) {
    textCh := make(chan string, 32)
    errCh := make(chan error, 1)

    go func() {
        defer close(textCh)
        defer close(errCh)

        if err := c.stream(ctx, messages, textCh); err != nil {
            if ctx.Err() != nil {
                return
            }
            errCh <- err
        }
    }()

    return textCh, errCh
}

func (c *Client) stream(ctx context.Context, messages []ai.Message, out chan<- string) error {
    payload := chatRequest{Model: c.model, Stream: true}
    for _, m := range messages {
        payload.Messages = append(payload.Messages, msgPayload{Role: m.Role, Content: m.Content})
    }

    body, _ := json.Marshal(payload)
    req, err := http.NewRequestWithContext(ctx, "POST",
        "https://openrouter.ai/api/v1/chat/completions",
        bytes.NewReader(body),
    )
    if err != nil {
        return err
    }
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Authorization", "Bearer "+c.apiKey)
    // OpenRouter 推荐加这两个 header，用于统计来源（可选）
    req.Header.Set("HTTP-Referer", "https://github.com/yourname/goclaw")
    req.Header.Set("X-Title", "goclaw")

    resp, err := c.http.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        var errBody struct {
            Error struct{ Message string `json:"message"` } `json:"error"`
        }
        json.NewDecoder(resp.Body).Decode(&errBody)
        return fmt.Errorf("openrouter %d: %s", resp.StatusCode, errBody.Error.Message)
    }

    // 解析 SSE 流（OpenAI 格式）
    // data: {"choices":[{"delta":{"content":"Hello"}}]}
    // data: [DONE]
    scanner := bufio.NewScanner(resp.Body)
    scanner.Buffer(make([]byte, 64*1024), 64*1024)

    for scanner.Scan() {
        line := scanner.Text()
        if !strings.HasPrefix(line, "data: ") {
            continue
        }
        data := strings.TrimPrefix(line, "data: ")
        if data == "[DONE]" {
            return nil
        }

        var chunk streamChunk
        if err := json.Unmarshal([]byte(data), &chunk); err != nil {
            continue
        }
        if len(chunk.Choices) == 0 {
            continue
        }
        if content := chunk.Choices[0].Delta.Content; content != "" {
            select {
            case out <- content:
            case <-ctx.Done():
                return nil
            }
        }
    }
    return scanner.Err()
}

// Phase 1 不需要注册表，直接在 main.go 里用 openrouter.New() 创建即可
// RegisterProvider / init() 注册模式在 Phase 6 引入多模型 fallback 时才加入
```

---

## 第五步：Telegram Bot

### types.go — API 数据结构

```go
// internal/telegram/types.go

package telegram

// Update 是 Telegram 推送给 Bot 的更新单元
type Update struct {
    UpdateID int      `json:"update_id"`
    Message  *Message `json:"message"`
}

type Message struct {
    MessageID int    `json:"message_id"`
    Chat      Chat   `json:"chat"`
    From      *User  `json:"from"`
    Text      string `json:"text"`
}

type Chat struct {
    ID   int64  `json:"id"`
    Type string `json:"type"` // "private" | "group" | "supergroup" | "channel"
}

type User struct {
    ID        int64  `json:"id"`
    FirstName string `json:"first_name"`
    Username  string `json:"username"`
}

// getUpdatesResponse 是 getUpdates API 的响应
type getUpdatesResponse struct {
    OK     bool     `json:"ok"`
    Result []Update `json:"result"`
}

// sendMessageResponse 是 sendMessage API 的响应
type sendMessageResponse struct {
    OK     bool    `json:"ok"`
    Result Message `json:"result"`
}
```

### bot.go — Bot 核心

```go
// internal/telegram/bot.go

package telegram

import (
    "context"
    "encoding/json"
    "fmt"
    "log"
    "net/http"
    "net/url"
    "strconv"
    "time"
)

// MessageHandler 是 Bot 收到消息后调用的处理函数
// 返回 string 作为回复内容（空字符串表示不回复）
type MessageHandler func(ctx context.Context, msg *Message) (<-chan string, <-chan error)

type Bot struct {
    token   string
    apiBase string
    handler MessageHandler
    client  *http.Client
}

func New(token string, handler MessageHandler) *Bot {
    return &Bot{
        token:   token,
        apiBase: "https://api.telegram.org/bot" + token,
        handler: handler,
        client:  &http.Client{Timeout: 30 * time.Second},
    }
}

// StartPolling 启动长轮询，阻塞直到 ctx 取消
func (b *Bot) StartPolling(ctx context.Context) error {
    log.Println("[telegram] starting long polling...")
    offset := 0

    for {
        select {
        case <-ctx.Done():
            log.Println("[telegram] polling stopped")
            return nil
        default:
        }

        updates, err := b.getUpdates(ctx, offset, 30) // 30秒长轮询超时
        if err != nil {
            if ctx.Err() != nil {
                return nil
            }
            log.Printf("[telegram] getUpdates error: %v, retrying in 3s...", err)
            // 退避重试：网络抖动时不要立刻重试
            select {
            case <-time.After(3 * time.Second):
            case <-ctx.Done():
                return nil
            }
            continue
        }

        for _, u := range updates {
            offset = u.UpdateID + 1
            if u.Message != nil && u.Message.Text != "" {
                // 每条消息独立 goroutine，互不阻塞
                go b.handleMessage(ctx, u.Message)
            }
        }
    }
}

func (b *Bot) handleMessage(ctx context.Context, msg *Message) {
    log.Printf("[telegram] message from %d: %s", msg.Chat.ID, msg.Text)

    // 先发送一条占位消息，获取 message_id 用于后续编辑
    placeholder, err := b.SendMessage(msg.Chat.ID, "…")
    if err != nil {
        log.Printf("[telegram] send placeholder failed: %v", err)
        return
    }

    // 调用处理器，获取流式文本 channel
    textCh, errCh := b.handler(ctx, msg)

    // 将流式输出写回 Telegram
    b.streamToTelegram(ctx, msg.Chat.ID, placeholder.MessageID, textCh, errCh)
}
```

### send.go — 发送与流式更新

```go
// internal/telegram/send.go

package telegram

import (
    "context"
    "encoding/json"
    "fmt"
    "log"
    "net/http"
    "net/url"
    "strconv"
    "strings"
    "time"
)

// SendMessage 发送新消息，返回消息对象（含 MessageID）
func (b *Bot) SendMessage(chatID int64, text string) (*Message, error) {
    params := url.Values{
        "chat_id":    {strconv.FormatInt(chatID, 10)},
        "text":       {text},
        "parse_mode": {"Markdown"},
    }
    var result sendMessageResponse
    if err := b.apiCall("sendMessage", params, &result); err != nil {
        return nil, err
    }
    return &result.Result, nil
}

// EditMessage 编辑已发送的消息（用于流式更新）
func (b *Bot) EditMessage(chatID int64, messageID int, text string) error {
    params := url.Values{
        "chat_id":    {strconv.FormatInt(chatID, 10)},
        "message_id": {strconv.Itoa(messageID)},
        "text":       {text},
        "parse_mode": {"Markdown"},
    }
    return b.apiCall("editMessageText", params, nil)
}

// SendTyping 发送"正在输入"状态
func (b *Bot) SendTyping(chatID int64) error {
    params := url.Values{
        "chat_id": {strconv.FormatInt(chatID, 10)},
        "action":  {"typing"},
    }
    return b.apiCall("sendChatAction", params, nil)
}

// streamToTelegram 将流式文本 channel 实时更新到 Telegram 消息
// 核心技巧：节流编辑，避免触发 Telegram 的频率限制（20次/分钟/消息）
func (b *Bot) streamToTelegram(
    ctx context.Context,
    chatID int64,
    messageID int,
    textCh <-chan string,
    errCh <-chan error,
) {
    var buf strings.Builder
    // 300ms 节流：每 300ms 最多编辑一次
    ticker := time.NewTicker(300 * time.Millisecond)
    defer ticker.Stop()

    lastSent := ""
    flush := func() {
        current := buf.String()
        if current == lastSent || current == "" {
            return
        }
        if err := b.EditMessage(chatID, messageID, current); err != nil {
            log.Printf("[telegram] edit message failed: %v", err)
        }
        lastSent = current
    }

    for {
        select {
        case chunk, ok := <-textCh:
            if !ok {
                // 流结束，最终刷新一次
                flush()
                return
            }
            buf.WriteString(chunk)

        case err := <-errCh:
            if err != nil {
                log.Printf("[telegram] AI stream error: %v", err)
                b.EditMessage(chatID, messageID, fmt.Sprintf("❌ 出错了：%v", err))
            }
            return

        case <-ticker.C:
            flush()

        case <-ctx.Done():
            flush()
            return
        }
    }
}

// getUpdates 调用 Telegram getUpdates API
func (b *Bot) getUpdates(ctx context.Context, offset, timeout int) ([]Update, error) {
    params := url.Values{
        "offset":          {strconv.Itoa(offset)},
        "timeout":         {strconv.Itoa(timeout)},
        "allowed_updates": {`["message"]`},
    }
    var result getUpdatesResponse
    if err := b.apiCall("getUpdates", params, &result); err != nil {
        return nil, err
    }
    return result.Result, nil
}

// apiCall 是所有 Telegram API 调用的底层方法
func (b *Bot) apiCall(method string, params url.Values, result any) error {
    apiURL := fmt.Sprintf("%s/%s?%s", b.apiBase, method, params.Encode())
    resp, err := b.client.Get(apiURL)
    if err != nil {
        return fmt.Errorf("telegram API %s: %w", method, err)
    }
    defer resp.Body.Close()

    if result == nil {
        return nil
    }
    return json.NewDecoder(resp.Body).Decode(result)
}
```

---

## 第六步：main.go 组装

```go
// main.go

package main

import (
    "context"
    "log"
    "os"
    "os/signal"
    "syscall"

    "github.com/yourname/goclaw/internal/ai/openrouter"
    "github.com/yourname/goclaw/internal/telegram"

    "gopkg.in/yaml.v3"
)

// Config 临时配置结构，Phase 4 会用专门的 config 包替换
type Config struct {
    Telegram struct {
        Token string `yaml:"token"`
    } `yaml:"telegram"`
    AI struct {
        APIKey       string `yaml:"api_key"`
        Model        string `yaml:"model"`
        SystemPrompt string `yaml:"system_prompt"`
    } `yaml:"ai"`
}

func main() {
    // 加载配置
    cfg := loadConfig("config.yaml")

    // 直接创建 OpenRouter 客户端，不走注册表
    // Phase 6 引入多模型 fallback 时再改成注册表模式
    aiClient := openrouter.New(cfg.AI.APIKey, cfg.AI.Model)

    // system prompt 通过 user message 前插 system message 传入
    // （OpenRouter/OpenAI 格式：messages 数组第一条 role=system）
    handler := func(ctx context.Context, msg *telegram.Message) (<-chan string, <-chan error) {
        // Phase 2 中这里会加入会话历史管理
        messages := []ai.Message{
            {Role: "system", Content: cfg.AI.SystemPrompt},
            {Role: "user", Content: msg.Text},
        }
        return aiClient.StreamChat(ctx, messages)
    }

    // 初始化 Telegram Bot
    bot := telegram.New(cfg.Telegram.Token, handler)

    // 优雅关闭：监听系统信号
    ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer cancel()

    log.Println("goclaw started. Send a message to your bot on Telegram.")
    if err := bot.StartPolling(ctx); err != nil {
        log.Fatal(err)
    }
    log.Println("goclaw stopped.")
}

func loadConfig(path string) *Config {
    data, err := os.ReadFile(path)
    if err != nil {
        log.Fatalf("failed to read config: %v", err)
    }
    var cfg Config
    if err := yaml.Unmarshal(data, &cfg); err != nil {
        log.Fatalf("failed to parse config: %v", err)
    }
    return &cfg
}
```

---

## 运行与验证

```bash
# 安装依赖（OpenRouter 使用标准库 HTTP，无需额外 SDK）
go get gopkg.in/yaml.v3

# 运行
go run main.go

# 验证
# 1. 打开 Telegram，找到你的 Bot
# 2. 发送任意文字
# 3. Bot 应该先显示 "…"，然后逐渐更新为 AI 的回复
```

---

## 本阶段核心工程知识点

### 1. Long Polling vs Webhook

| 方式 | 原理 | 适合场景 |
|------|------|----------|
| Long Polling | Bot 主动请求 Telegram，服务器保持连接直到有消息 | 本地开发，无公网 IP |
| Webhook | Telegram 主动 POST 给你的服务器 | 生产部署，有公网 IP |

本阶段用 Long Polling，Phase 3 加 Gateway 后可以切换到 Webhook。

### 2. SSE（Server-Sent Events）解析

OpenRouter 使用 OpenAI 兼容格式，比 Anthropic 原生格式更简洁：
```
data: {"id":"...","choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}
data: {"id":"...","choices":[{"delta":{"content":" world"},"finish_reason":null}]}
data: [DONE]
```
关键：只需要取 `choices[0].delta.content`，看到 `[DONE]` 结束。

**与 Anthropic 原生格式的区别：**

| | OpenRouter（OpenAI 兼容）| Anthropic 原生 |
|--|--|--|
| 端点 | `/v1/chat/completions` | `/v1/messages` |
| 认证 | `Authorization: Bearer sk-or-...` | `x-api-key: sk-ant-...` |
| System prompt | messages 数组第一条 `role: system` | 独立的 `system` 字段 |
| 流式文本字段 | `choices[0].delta.content` | `delta.text`（需判断 type） |
| 结束标志 | `data: [DONE]` | `event: message_stop` |

### 3. goroutine 生命周期管理

```
main goroutine
├── StartPolling goroutine（长期运行，直到 ctx 取消）
│   └── handleMessage goroutine（每条消息一个，短期）
│       └── streamToTelegram（在 handleMessage goroutine 内串行执行）
└── signal 监听（触发 ctx 取消，引发级联关闭）
```

**关键原则：** goroutine 必须有退出条件，`ctx.Done()` 是标准退出信号。

### 4. Telegram 频率限制

Telegram 限制：同一消息编辑频率 **最多 20次/分钟**，全局 **30次/秒**。
解决方案：`time.Ticker(300ms)` 节流，节流间隔内累积文本再一次性编辑。

---

## 与 OpenClaw 的对应关系

| 本阶段代码 | OpenClaw 对应 |
|-----------|---------------|
| `bot.StartPolling` | `src/channels/telegram/` 的消息监听循环 |
| `streamToTelegram` | `runReplyAgent()` 中 typing indicator + 流式发送逻辑 |
| `ai.Client` interface | `src/agents/pi-embedded-runner/` 的模型调用抽象 |
| `handler` 函数 | `server-chat.ts` 的 `chat.send` 处理器 |

---

## 下一阶段预告

Phase 1 的 `handler` 每次都只用当前一条消息，没有历史。
Phase 2 将加入 **SessionStore**，让每个用户拥有独立的对话历史，重启后不丢失。
