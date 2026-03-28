package tools

import (
	"sync"
)

type Registry struct {
	mu    sync.RWMutex
	tools map[string]*Tool
}

func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]*Tool),
	}
}
func (r *Registry) Register(t *Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name] = t
}
func (r *Registry) Get(name string) (*Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[name]
	return tool, ok
}

// Definitions 导出给 AI 的工具描述列表
// AI 通过这个列表知道有哪些工具可以调用
func (r *Registry) Definitions() []map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]map[string]any, 0, len(r.tools))
	for _, tool := range r.tools {
		result = append(result, map[string]any{
			"name":         tool.Name,
			"description":  tool.Description,
			"input_schema": tool.InputSchema,
		})
	}
	return result
}

func (r *Registry) FilterForAgent(agentID string) *Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	filterRegistry := NewRegistry()
	for _, tool := range r.tools {
		if isAllowed(tool, agentID) {
			filterRegistry.Register(tool)
		}
	}
	return filterRegistry
}

func isAllowed(t *Tool, agentID string) bool {
	//全部允许
	if len(t.Policy.AllowedAgents) == 0 {
		return true
	}
	for _, id := range t.Policy.AllowedAgents {
		if id == agentID {
			return true
		}
	}
	return false
}
