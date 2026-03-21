# Phase 3 回顾：Gateway WebSocket RPC 服务器

## 完成了什么

构建了一个本地 Gateway 服务器，通过 WebSocket 接受客户端连接，处理 RPC 请求，调用 AI，把结果广播回去。

## 真正理解的东西

### WebSocket 升级
HTTP 握手 → 101 Switching Protocols → 升级成管道。
握手时做鉴权（Auth），升级后就是持久连接，不再有 HTTP header。

### readPump / writePump 分工
一个 WebSocket 连接用两个 goroutine：
- readPump：阻塞等消息，收到就交给 Router 处理，结果塞进 client.send
- writePump：监听 client.send，有数据就写进 WebSocket

两者通过 client.send channel 通信，不共享 conn 的写操作（WebSocket 写不是并发安全的）。

### Handler 的双流程
这是这个 Phase 最核心的模式：

```
readPump → Router → handler
                       ↓ 立刻返回 ResponseFrame（started）→ client.send → writePump → Bot
                       ↓ 同时开 goroutine → AI 流式输出 → hub.BroadCast → EventFrame → Bot
```

一个 handler 同时做两件事：点对点响应 + 广播事件。

### channel 作为状态传递的核心手段
这个 Phase 大量用到同一个模式：
- 有一个主体在 for+select 里监听多个 channel
- 外部通过往 channel 塞数据来触发行为
- 不共享内存，只传递消息

Hub.Run()、writePump、readPump 都是这个模式。

### select 竞争 bug
当 rawCh 和 errCh 同时关闭，select 随机选 case。
选到 errCh（nil）就直接 return，fullReply 还是空的。
修复：用 for range rawCh 先读完，再单独检查 errCh，顺序固定。

### r.Context() 生命周期
HTTP 请求的 context 在 ServeWS 返回后就取消。
goroutine 里用这个 ctx 调 AI，请求立刻被取消。
修复：用 context.Background() 给 WebSocket 连接独立的生命周期。

## 还模糊的地方

- 多个变量联动的数据结构设计，需要全局视角才能想清楚
- WebSocket 服务器的实现细节（upgrader、deadline、Ping/Pong）靠文档，自己设计还有难度
- 复杂系统里各组件的职权划分，需要更多实践才能形成直觉

## 核心模式总结

```
ws 链接 Bot ←→ Gateway（Hub + Router + Handler）←→ AI
                         ↑
                   client.send 是信息传递的核心通道
```

Go 的并发模式：不要通过共享内存来通信，要通过通信来共享内存。
这个 Phase 把这句话具体化了。
