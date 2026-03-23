# Phase 4 回顾：配置系统 + 热重载

## 完成了什么

建立了 `internal/config/` 包，实现结构化配置加载、验证、热重载。

## 为什么要单独建 config 包

`main` 包不能被其他包 import。原来 `Config` 定义在 `main.go`，`gateway`、`telegram` 等包只能靠 `main.go` 把值一个个拆开传进去，字段一多函数签名就越来越长。

把 `Config` 挪到 `internal/config/`，所有包都能 import，`main.go` 只负责加载，其他包拿 `*config.Config` 直接用。

## 热重载的触发流程

```
修改 config.yaml 并保存
        ↓
fsnotify 收到文件系统事件，塞进 watcher.Events channel
        ↓
Watch() 的 select 读到事件
        ↓
Write / Create → reload()
Remove / Rename → watcher.Add(path)（重新登记监听，防止 vim 保存后监听失效）
        ↓
reload() 重新 Load(path) → ptr.Store(newCfg) → 通知所有 handlers
        ↓
下次 Get() 拿到新配置
```

## atomic.Pointer vs channel

Phase 3 的 Hub 用纯 channel 通信——只有 `Hub.Run()` 一个 goroutine 操作 `clients` map，外部通过 channel 传消息给它，不直接碰 map，不需要锁。

config Manager 用 `atomic.Pointer`——多个 goroutine 频繁读配置，偶尔有一个 goroutine 写。atomic 是 CPU 原子指令，Load/Store 一条指令完成，比 RWMutex 更轻量。

两种并发手段适用场景不同：
- channel：传递消息，生产者消费者模式
- atomic：保护共享数据的读写，多读少写场景

## 哪些字段能热更新，哪些需要重启

判断原则：**这个字段是启动时绑定一次，还是每次请求时实时读？**

- 启动时绑定一次（port、telegram token）→ 改了没用，要重启对应组件
- 每次请求时从 `cfgMgr.Get()` 读（system_prompt、max_context_pairs）→ 改了下次自动生效

## fsnotify 的 vim 陷阱

vim 保存文件不是直接写入，而是：先写临时文件 → 删除原文件 → 重命名临时文件。

删除原文件会触发 `Remove` 事件，fsnotify 对原文件的监听句柄失效。
解决：收到 `Remove`/`Rename` 后重新 `watcher.Add(path)`，对新文件重新登记。

## 核心模式

```
Get()    → ptr.Load()  → 直接读当前快照，无锁，O(1)
reload() → ptr.Store() → 原子替换指针，下次 Get() 自动拿到新值
```

配置对象是只读快照，更新只通过 `reload()` 进行，调用方永远拿到完整的配置，不会读到写到一半的状态。
