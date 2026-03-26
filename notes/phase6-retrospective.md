# Phase 6 回顾：Agent 抽象 + 多模型 Fallback + 中止机制

## 完成了什么

建立了 `internal/agent/` 包，把 AI 推理逻辑从 Gateway 完全抽离。引入 AbortRegistry 支持中止推理，runWithFallback 支持模型故障自动切换，AgentRegistry 管理多个 Agent 实例。同时修复了 13 个 bug。

## 为什么要抽出 Agent 层

Phase 5 的 `ChatHandler` 直接持有 `aiClient`，推理逻辑全在 Gateway 里：

```
ChatHandler.handleChat → aiClient.StreamChat → hub.Broadcast
```

问题：
- 只有一个全局 AI 客户端，无法多模型
- 没有 abort 机制
- 无法针对不同场景（代码、通用）配置不同系统提示

Phase 6 引入 Agent 层后：

```
ChatHandler.Send → AgentRegistry.Get(agentID) → Agent.RunReply
                                                    → runWithFallback → runAttempt → AI API
                                                    → eventCh（事件流出）
ChatHandler 读 eventCh → Hub.Broadcast
```

Gateway 不再关心 AI 细节，只负责路由。

## 三层执行链

```
RunReply          ← 用户体验层：注册 abort，写入 Session
  runWithFallback ← 可靠性层：主模型失败换备用
    runAttempt    ← 执行层：调 AI API，输出事件流
```

每层职责独立，可以单独测试和替换。

## 用 channel 解耦推理和广播

推理结果通过 eventCh 传递，不直接依赖 Hub：

```go
eventCh := make(chan AgentEvent, 64)
go func() {
    for e := range eventCh {
        hub.Broadcast(...)   // Gateway 负责广播
    }
}()
go func() {
    defer close(eventCh)
    agent.RunReply(..., eventCh)  // Agent 只管写
}()
```

Agent 包里没有任何 Gateway 代码。以后换成 gRPC 推送，Agent 代码不用改。

buffer=64 + sendEvent 里的 default 分支：推理不能因为广播慢而阻塞，delta 事件丢几个可以接受，推理卡住不能接受。

## AbortRegistry 的本质

维护 `runID → cancel` 的映射：

```
RunReply 开始 → Register(ctx, runID) → 存入 cancel
外部 Abort(runID)                   → 找到 cancel，调它
RunReply 结束 → Unregister(runID)   → 清理
```

cancel 被调用后，传进 StreamChat 的 ctx 取消，`<-ctx.Done()` 触发，推理停止。这是 context 树形传播的直接应用。

## 依赖注入顺序

Phase 6 的循环依赖比 Phase 5 更长：

```go
gw := gateway.New(cfgMgr, store)           // agentReg 暂时 nil
chanMgr := channel.NewManager(gw.InboundHandler())
gw.SetChannelManager(chanMgr)
agentReg := agent.NewRegistry(cfgMgr, store, chanMgr)
gw.SetAgentRegistry(agentReg)              // 最后注入
```

规律：先建最核心的（Gateway），再建依赖它的（chanMgr、agentReg），最后注入回去。

## AI 模型注册表

和 channel 注册表同一个模式：

```
openrouter.init() → ai.RegisterModelFactory("openrouter", ...)
main.go blank import → 触发 init()
runAttempt → ai.NewClient("openrouter", apiKey, model)
```

runAttempt 每次临时创建 client，不复用。代价是每次推理分配一个结构体，好处是 fallback 时可以切换任意模型，不受初始化时的绑定限制。

## Bug 修复中学到的

5 个中高风险 bug 归结为两类认知：

**运行时行为类（靠积累）**
- channel 只能 close 一次，close 职责要唯一
- context.Background() 的 Done() 永远返回 nil，程序无法退出
- ctx 截断意味着取消信号丢失

**外部数据不可信（靠习惯）**
- API 返回的指针字段一律检查 nil
- 外部输入 Unmarshal 失败要处理，不能用零值继续

## 下一步

Phase 7：Tools（工具调用）。Agent 目前只能生成文字，Phase 7 让 AI 可以调用注册的函数（搜索、读文件），实现多步推理。
