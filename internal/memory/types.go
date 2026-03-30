// internal/memory/types.go

package memory

import "time"

// Entry 是一条记忆记录
type Entry struct {
	ID        int64
	AgentID   string   // 属于哪个 Agent
	SessionID string   // 来自哪个会话（可选，用于追溯）
	Content   string   // 记忆内容（自然语言描述）
	Tags      []string // 标签（用于过滤）
	Source    string   // 来源："user_message" | "ai_extract" | "manual"
	CreatedAt time.Time
	UpdatedAt time.Time

	// 检索时填充
	Score float64 // 综合相关性分数
}

// SearchQuery 是记忆检索请求
type SearchQuery struct {
	AgentID    string
	Query      string   // 查询文本
	Tags       []string // 标签过滤（可选）
	Limit      int      // 返回数量上限（默认 5）
	MaxAgeDays int      // 只检索最近 N 天的记忆（0=不限）
}

// Store 是记忆存储的接口
type Store interface {
	// Save 保存一条记忆
	Save(entry *Entry) error

	// Search 检索相关记忆（BM25 + 时间衰减混合排序）
	Search(query SearchQuery) ([]*Entry, error)

	// Delete 删除指定记忆
	Delete(id int64) error

	// List 列出所有记忆（用于管理）
	List(agentID string, limit, offset int) ([]*Entry, error)

	// Count 统计记忆数量
	Count(agentID string) (int64, error)

	// Close 关闭存储
	Close() error
}
