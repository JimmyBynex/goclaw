# Phase 2 实操复盘

## 完成内容

多轮对话短期记忆 + 多用户独立 Session 文件持久化。

未完成：`Reset()`、`ShouldReset()` 等 session 方法暂未实现；handler 仍耦合在 `main.go`（Phase 3 再拆）。

---

## 心智模型

### Session vs Store

- **Session** = 数据格式，一个用户的对话历史文件，本身只是结构体
- **Store** = 主体，管文件的工具，负责读写缓存

更倾向于以 Store 为中心思考：Store 持有所有 Session，外界只和 Store 交互（Get/Save/Delete），不直接操作文件。

### SessionKey

唯一标识一个对话，序列化成文件名：

```
telegram__bot001__dm__6644154372__default.json
```

五个维度：渠道、账号、隔离级别、对端ID、代理ID。现阶段只用 dm/group，AccountID 和 AgentID 固定值，为后续 phase 预留。

### 读写锁（RWMutex）

两把锁的核心规则：
- **读锁**（RLock）：快，不 defer，多个 goroutine 可以同时持有
- **写锁**（Lock）：慢，defer，独占

关键：写锁拿到后必须 **double-check cache**，防止两个 goroutine 同时 miss 读锁、同时进入写锁、重复读文件。

### 流的生产消费关系

流本质是生产消费：AI 生产 chunk，streamToTelegram 消费。

**Tap Pattern**：在流中间插一个 goroutine，边转发边收集完整回复，流结束后写回 Session：

```
AI → rawTextCh → goroutine → textCh → streamToTelegram → Telegram
                     ↓
              收集完整回复 → AddAssistantMessage → store.Save
```

---

## 遇到的 Bug

### Bug 1：值类型还是指针类型不清晰

**现象**：不确定函数该返回 `Session` 还是 `*Session`
**规则**：会被修改的、字段多的用指针；小且不变的（如 SessionKey、ai.Message）用值类型
**教训**：问自己"这个东西会被修改吗"，会变就用指针

---

### Bug 2：Windows 文件名不能含冒号

**现象**：`rename` 报错 "The parameter is incorrect"
**原因**：用 `cfg.Telegram.Token` 作为 AccountID，token 里含有 `:`，Windows 文件名不允许冒号
**修法**：`config.yaml` 单独加 `account_id: "bot001"` 字段，不用 token 作文件名
**教训**：文件名要用安全字符，敏感凭证不应混入业务标识符

---

### Bug 3：textCh 被提前消费

**现象**：streamToTelegram 收到空 channel，消息不显示
**原因**：在 handler 里用 `for range` 消费完 rawTextCh 再 return，channel 已关闭
**修法**：改用 Tap Pattern，goroutine 边转发边收集，return 的是新 channel
**教训**：channel 只能被消费一次，需要多处使用时必须 Tap 或 fan-out

---

## 核心工程知识点

- **原子文件写入**：先写 `.tmp`，再 `os.Rename`，防止写到一半崩溃导致文件损坏
- **double-check lock**：读锁 miss → 写锁 → 再检查一次，防止重复磁盘 IO
- **Tap Pattern**：在 channel 流中插观察者，不改变流方向，同时收集副作用
