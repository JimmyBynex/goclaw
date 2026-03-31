package gateway

import (
	"context"
	"encoding/json"
	"goclaw/internal/memory"
)

type MemoryHandler struct {
	store   memory.Store
	agentID string
}

// memory.search：搜索记忆（RPC 方法）
func (h *MemoryHandler) Search(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		AgentID string `json:"agent_id"`
		Query   string `json:"query"`
		Limit   int    `json:"limit"`
	}
	json.Unmarshal(raw, &p)
	if p.Limit <= 0 {
		p.Limit = 10
	}
	entries, err := h.store.Search(memory.SearchQuery{
		AgentID: p.AgentID,
		Query:   p.Query,
		Limit:   p.Limit,
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// memory.save：手动保存一条记忆
func (h *MemoryHandler) Save(ctx context.Context, raw json.RawMessage) (any, error) {
	var e memory.Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, err
	}
	if err := h.store.Save(&e); err != nil {
		return nil, err
	}
	return e, nil
}

// memory.delete：删除一条记忆
func (h *MemoryHandler) Delete(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(raw, &p)
	return nil, h.store.Delete(p.ID)
}

// memory.list：列出所有记忆（管理用）
func (h *MemoryHandler) List(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		AgentID string `json:"agent_id"`
		Limit   int    `json:"limit"`
		Offset  int    `json:"offset"`
	}
	json.Unmarshal(raw, &p)
	if p.Limit <= 0 {
		p.Limit = 20
	}
	return h.store.List(p.AgentID, p.Limit, p.Offset)
}
