package agent

import (
	"errors"
	"goclaw/internal/channel"
	"goclaw/internal/config"
	"goclaw/internal/memory"
	"goclaw/internal/session"
	"log"
	"sync"
)

// 与前面的registry的不太一样，前面的registry存储的是新建方法，所以直接用var一个map就可以了
// 这里的角色更像是manager，实现并管理agent实例
type Registry struct {
	mu       sync.RWMutex
	agents   map[string]*Agent
	abortReg *AbortRegistry
}

// 这个函数只是注册本身以及注册热加载
func NewRegistry(cfgMgr *config.Manager, store session.Store, chanMgr *channel.Manager, memoryMgr *memory.Manager) *Registry {
	r := &Registry{
		agents:   make(map[string]*Agent),
		abortReg: NewAbortRegistry(),
	}
	r.reloadAgents(cfgMgr.Get(), store, chanMgr, memoryMgr)
	cfgMgr.OnChange(func(old, new *config.Config) {
		r.reloadAgents(cfgMgr.Get(), store, chanMgr, memoryMgr)
	})
	return r

}

// 实际导入
func (r *Registry) reloadAgents(cfg *config.Config, store session.Store, chanMgr *channel.Manager, memoryMgr *memory.Manager) {
	newAgents := make(map[string]*Agent)
	for _, agent := range cfg.Agents {
		newAgents[agent.ID] = FromConfig(agent, cfg.AI, store, r.abortReg, chanMgr, memoryMgr)
	}
	if len(newAgents) == 0 {

		newAgents["default"] = FromConfig(config.AgentConfig{ID: "default"}, cfg.AI, store, r.abortReg, chanMgr, memoryMgr)
	}
	r.mu.Lock()
	r.agents = newAgents
	r.mu.Unlock()
}

func (r *Registry) Get(agentID string) (*Agent, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	agent, ok := r.agents[agentID]
	if !ok {
		log.Printf("[agent]agent %s not found", agentID)
		return nil, errors.New("agent not found")
	}
	return agent, nil
}

func (r *Registry) Abort(runID string) bool {
	return r.abortReg.Abort(runID)
}
