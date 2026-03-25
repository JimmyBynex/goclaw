package agent

import (
	"context"
	"sync"
)

// AbortRegistry 维护 runID → cancel 的映射
// 允许外部（如 chat.abort RPC 方法）中止正在进行的推理
type AbortRegistry struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func NewAbortRegistry() *AbortRegistry {
	return &AbortRegistry{
		cancels: make(map[string]context.CancelFunc),
	}
}

func (r *AbortRegistry) Register(parent context.Context, runID string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	r.mu.Lock()
	r.cancels[runID] = cancel
	r.mu.Unlock()
	return ctx, cancel
}

func (r *AbortRegistry) Unregister(runID string) {
	r.mu.Lock()
	delete(r.cancels, runID)
	r.mu.Unlock()
}

// Abort 中止指定 runID 的运行
// 返回 true 表示成功中止，false 表示 runID 不存在（已完成或从未存在）
func (r *AbortRegistry) Abort(runID string) bool {
	r.mu.Lock()
	cancel, ok := r.cancels[runID]
	r.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// ActiveRuns返回当前正在运行的RunID列表
func (r *AbortRegistry) ActiveRuns() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for id := range r.cancels {
		ids = append(ids, id)
	}
	return ids
}
