# Phase 5 回顾：Channel 抽象 + ChannelManager

## 完成了什么

建立了 `internal/channel/` 包，定义标准化消息类型和 Channel 接口，重构 Telegram 实现，引入 ChannelManager 统一管理渠道生命周期，把 Telegram 消息路径接入 Gateway。

## 为什么要抽象 Channel

原来 Telegram 消息走独立的 `makeHandler`，完全绕过 Gateway：

```
Telegram → makeHandler (main.go) → AI → 流回 Telegram
```

Gateway 不知道这条消息存在，CLI 工具看不到流，也没办法 abort。

抽象后所有消息都经过 Gateway：

```
Telegram → InboundMessage → Gateway → AI → Hub 广播（WebSocket 客户端）
                                          → Bot.SendStream（Telegram 用户）
```

## 双线输出

两条出口职责不同，不能合并：

- **Hub 广播**：给连着 Gateway WebSocket 的客户端（CLI、手机 App）推事件
- **ChannelManager**：调平台 API 把回复发回 Telegram/Discord

Telegram 用户没有连接到 Gateway，只认 Telegram 服务器的 API，所以必须单独一条出口。

## 依赖循环的解法

Gateway 和 ChannelManager 互相依赖：

```
Gateway.InboundHandler → 需要知道往哪里发回（chanMgr）
ChannelManager → 需要 InboundHandler 才能创建
```

解法：先建 Gateway（不传 chanMgr），再建 ChannelManager，再把 chanMgr 注入回 Gateway：

```go
gw := gateway.New(cfgMgr, aiClient, store)          // chanMgr 暂时为 nil
chanMgr := channel.NewManager(gw.InboundHandler())  // 用 gw 的 handler
gw.SetChannelManager(chanMgr)                        // 注入回去
```

## 工厂函数 + init() 自注册

Registry 存的是"如何创建 Channel"，不是 Channel 实例本身：

```go
// telegram 包的 init()，被 import 时自动执行
func init() {
    channel.Register("telegram", func(...) (Channel, error) {
        return New(...)
    })
}

// main.go 只需要 blank import
import _ "goclaw/internal/channel/telegram"
```

好处：main.go 不需要知道有哪些渠道，加新渠道只需要加一行 import。

## 小接口 + 能力检测

`Channel` 核心接口只有 6 个方法。流式发送是可选能力，用独立接口：

```go
type StreamSender interface {
    SendStream(ctx context.Context, peerID string, textCh <-chan string) error
}

// 发回复时检测是否支持
if ss, ok := ch.(channel.StreamSender); ok {
    go ss.SendStream(ctx, peerID, textCh)
} else {
    ch.Send(ctx, OutboundMessage{Text: fullReply})
}
```

SMS 渠道不支持编辑消息，不实现 `StreamSender`，自动降级一次性发送。

## 还没解决的问题

现在 `ChatHandler` 直接持有 `chanMgr`，多个 Agent 场景下路由逻辑会变复杂。Phase 6 的 Agent 抽象会把这条链路解耦，`chanMgr` 移进 Agent 里，ChatHandler 变成分发器。
