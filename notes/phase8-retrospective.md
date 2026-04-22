# Phase 8 回顾：Memory 长期记忆系统

## 完成了什么

建立了 `internal/memory/` 包，实现两层架构：
- 底层 `SQLiteStore`：基础 CRUD（Save、Delete、List、Count），FTS5 全文检索
- 业务层 `Manager`：`InjectMemories`（注入记忆到消息历史）、`ExtractAndSave`（提取并保存记忆）

将 memory 集成到 agent：推理前注入相关记忆，回复后异步提取保存。

---

## 两层架构的边界

```
SQLiteStore  ← 存储层：只管读写数据库，不懂业务
Manager      ← 业务层：知道怎么注入、提取，依赖 Store 接口
Agent        ← 推理层：只调 Manager，不关心底层怎么存
```

`Manager` 依赖 `Store` 接口而不是 `SQLiteStore`，测试时可以换 mock，将来换向量数据库只改底层实现。

---

## FTS5 的核心设计：为什么不用触发器

最直接的方案是 `content=memories`，让 FTS5 直接读主表内容，用触发器自动同步：

```sql
CREATE VIRTUAL TABLE memories_fts USING fts5(content, content=memories);
```

但这样 FTS5 存的是原始内容，不是分词结果。默认分词器按空格切词，对中文无效——"我在学Go并发编程"整句被当作一个词，搜"并发"什么也找不到。

所以改成手动双写：
```
主表 memories      存原始内容（"我在学Go并发编程"）← 展示给用户
虚拟表 memories_fts 存分词结果（"我 在 学 Go 并发 编程"）← 用于搜索
```

Save() 里两步写入，rowid 对齐，中间要 tokenize 再回写 id：

```go
result, _ := s.db.Exec("INSERT INTO memories ...")
id, _ := result.LastInsertId()
tokenized := s.tokenize(e.Content)
s.db.Exec("INSERT INTO memories_fts(rowid, content, tags) VALUES (?, ?, ?)", id, tokenized, ...)
```

---

## 搜索的两层宽松

FTS5 默认是 AND 匹配，所有词都要命中。用户查"我是谁"，存储的记忆是"我是计算机专业大二学生"，"谁"不在记忆里，AND 匹配失败，什么都找不到。

两层宽松解决这个问题：

```
查询："学习Go并发"
→ 分词：["学习", "Go", "并发"]
→ 加 *：["学习*", "Go*", "并发*"]   ← 第一层：词的部分匹配（"学习"能匹配"学习资料"）
→ OR：  "学习* OR Go* OR 并发*"     ← 第二层：词之间任一命中即可
```

缺第一层，输入"学"找不到"学习"；缺第二层，输入"我是谁"找不到"我是大二学生"。

---

## 倒排索引和 B 树索引的分工

```sql
WHERE memories_fts MATCH ?    -- FTS5 倒排索引：找哪些行包含查询词
  AND m.agent_id = ?          -- B 树索引：过滤属于这个 agent 的行
```

两个过滤条件，两套机制各管一块。`idx_memories_agent` 对 List() 全表扫描时更有意义。

---

## 现阶段的粗糙之处

**提取：** 规则/专家系统，只能匹配固定关键词（"记住"、"我是"、"我喜欢"）。漏掉大量值得记忆的信息。OpenClaw 用 AI 提取，对话结束后让 AI 分析对话生成结构化摘要。

**搜索：** BM25 关键词匹配，语义理解为零。"我是Go开发者"和"我在学Golang"匹配不到。OpenClaw 加了 70% 向量检索 + 30% BM25 的混合检索。

---

## 下一步

Phase 9：多渠道接入（Discord），验证 Channel 抽象层的开闭原则——接入新渠道时 Gateway、Agent、Session、Memory 代码一行都不改。
