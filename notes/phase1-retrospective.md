# Phase 1 实操复盘

## 完成内容

Telegram Bot + OpenRouter 流式对话，单轮无记忆，长轮询接收消息，流式编辑回复。

---

## 心智模型

### 整体架构
Telegram Bot 是主体（主服务器），AI 是一个抽象函数（输入文本，输出文本流）。Bot 不关心 AI 内部细节，只关心调用接口。

```
用户消息 → Telegram → getUpdates → handleMessage → AI.StreamChat → streamToTelegram → EditMessage → 用户看到回复
```

### 数据分层
- `Update`：Telegram 系统层，表示"有一个新事件"，用 `update_id` 追踪处理进度
- `Message`：业务层，表示"用户发了一条消息"，用 `message_id` 编辑回复

### offset 机制
不是每次 +1，是 `offset = 最后处理的 update_id + 1`。告诉 Telegram "只给我这个 ID 之后的更新"，避免重复处理。

### 流式节流
AI 输出很快，但 Telegram 限制编辑频率（约 20次/分钟/消息）。
解法：chunk 来了只存 buf，不立刻 edit；ticker 每 300ms 触发一次才 edit，且只有内容变化才发请求（`lastSent` 对比）。

### channel 关闭顺序
`StreamChat` 里两个 defer 是 LIFO 执行，后声明的先执行。`textch` 要先关，`errch` 后关，否则 `streamToTelegram` 可能在 textCh 排空前就 return。

---

## 遇到的 Bug

### Bug 1：`json:"message"` 字段名错误
**现象**：OpenRouter 返回 400
**原因**：OpenRouter API 字段名是 `messages`（复数），写成了 `message`
**修法**：`chatRequest` struct 里改成 `json:"messages"`
**教训**：对接外部 API 时，字段名必须和文档完全一致，复数单数都要注意

---

### Bug 2：`"Bearer"+apiKey` 缺空格
**现象**：OpenRouter 返回 401
**原因**：Authorization header 格式是 `Bearer <空格> token`，少了空格
**修法**：`"Bearer " + c.apiKey`
**教训**：字符串拼接时注意空格，认证头格式固定

---

### Bug 3：HTTP client timeout 太短
**现象**：`context deadline exceeded (Client.Timeout exceeded while awaiting headers), retrying...`
**原因**：长轮询 timeout=30s，HTTP client timeout 也是 30s，client 先超时断开
**修法**：HTTP client timeout 改成 35s，比轮询 timeout 长
**教训**：长轮询的 HTTP client timeout 必须 > 轮询 timeout

---

### Bug 4：出错直接退出轮询
**现象**：问几条消息后 bot 停止响应
**原因**：`StartPolling` 里 `getUpdates` 出错直接 `return err`，整个轮询退出
**修法**：改成 `log + time.Sleep(3s) + continue`，重试而不是退出
**教训**：网络服务的错误处理要用重试，不能一出错就退出

---

### Bug 5：消息截断
**现象**：AI 回复不完整，后半段丢失
**原因 1**：`errCh` case 收到 nil（channel 正常关闭）直接 return，textCh 里还有未处理的 chunk
**原因 2**：defer 顺序错误，`errch` 比 `textch` 先关闭，select 随机选中 errCh case 就 return 了
**修法 1**：`StreamChat` 里交换两行 defer，让 `textch` 先关
**修法 2**：`errCh` case 收到 nil 时设 `errCh = nil`（nil channel 不会被 select 选中），不 return，等 textCh 排空
**教训**：defer 是 LIFO，关闭顺序影响对端 select 行为；nil channel 是控制 select 的常用技巧

---

### Bug 6：Markdown 渲染问题
**现象**：表格、`#` 标题无法渲染，显示原始符号
**原因**：Telegram Markdown（v1）不支持标准 Markdown 的表格和标题语法
**修法**：system prompt 告诉 AI 不用 Markdown，或去掉 parse_mode 发纯文本
**教训**：Telegram Markdown 是子集，不等于 GitHub Flavored Markdown

---

## 排查 Bug 的策略

1. **顺着数据流走**：从数据产生到消费，每一步问"这里会不会提前结束/丢数据"
2. **看 defer/goroutine 顺序**：并发 bug 多出在执行顺序和预期不符，遇到 defer 就问 LIFO 谁先跑
3. **加 log**：在 channel 关闭、flush、return 等关键点打 log，运行一次顺序一目了然
4. **看 HTTP 状态码**：400 查字段名，401 查认证，先确认网络层是否通
5. **最小复现**：不用真实服务，直接在 main 里造假数据验证单个函数行为
