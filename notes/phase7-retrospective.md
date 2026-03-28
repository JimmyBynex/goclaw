# Phase 7 回顾：Tools 工具调用

## 完成了什么

建立了 `internal/tools/` 包，实现工具注册、执行、权限过滤。新增三个内置工具（get_current_time、calculate、http_fetch）。扩展 `ai.Client` 接口加入 `Chat` 方法，在 `runAttempt` 中实现工具调用循环，让 AI 可以主动调用外部函数完成多步推理。

## 工具调用的本质是一个协议

AI 和代码之间约定好两件事：
- AI 用固定格式告诉你"调哪个工具、传什么参数"（ToolUseBlock）
- 你用固定格式把结果告诉 AI（ToolResultBlock）

循环就是把这个协议跑通：AI 说话 → 你执行 → AI 再说话，直到 AI 不再要求调工具为止。

## 协议适配层

`openrouter.go` 的 `Chat` 方法做的事是协议适配，不是业务逻辑：

```
内部格式（ai.Message） → OpenAI API 格式（HTTP JSON）→ 内部格式（tools.Response）
```

三种消息形态（普通文字、AI 工具调用请求、工具执行结果）在内部用同一个 `ai.Message` 表达，发给 API 时转成 OpenAI 要求的格式。换一个 AI 提供商只需要改这一层，上层 runner 不动。

## 三层函数的边界

```
RunReply          ← 持久化边界：只把最终的 user/assistant 消息写入 Session
  runWithFallback ← 可靠性边界：模型失败切换备用
    runAttempt    ← 推理边界：工具调用循环在这里，loopMessages 临时存在于此
```

loopMessages 包含完整的工具调用链（AI 请求 + 工具结果），但推理结束后直接丢弃，不写入持久化 Session。Session 只保留用户消息和最终回答，保持干净，不浪费上下文窗口。

## for range 地址坑

```go
// 错误
for _, msg := range messages {
    payload.Messages = append(payload.Messages, msgPayload{Content: &msg.Content})
}

// 正确
for _, msg := range messages {
    content := msg.Content  // 复制一份
    payload.Messages = append(payload.Messages, msgPayload{Content: &content})
}
```

for range 的循环变量 `msg` 地址不变，每次只是把新值赋进去。存 `&msg.Content` 意味着所有消息指向同一个地址，循环结束后全变成最后一个值。先复制再取地址，每次都是独立变量。

## 下一步

Phase 8：Memory（记忆系统）。当前 AI 的记忆仅限于当前对话的消息列表，Phase 8 引入 SQLite + FTS5 全文检索，让 AI 可以存储和检索长期记忆，突破上下文窗口限制。
