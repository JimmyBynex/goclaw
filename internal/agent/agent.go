package agent

import (
	"goclaw/internal/channel"
	"goclaw/internal/config"
	"goclaw/internal/session"
)

// AI代理的核心
// 对话代理的实例，包括用什么模型，提示词，存储，中止
type Agent struct {
	id           string
	systemPrompt string
	models       []ModelRef
	store        session.Store
	abortReg     *AbortRegistry
	channelMgr   *channel.Manager //推理期间发 typing indicator
}
type ModelRef struct {
	Provider string
	APIkey   string
	Model    string
}

func FromConfig(
	agentCfg config.AgentConfig,
	globalAI config.AIConfig,
	store session.Store,
	abortReg *AbortRegistry,
	chanMgr *channel.Manager) *Agent {
	model := agentCfg.Model
	if model == "" {
		model = globalAI.Model
	}
	systemPrompt := agentCfg.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = globalAI.SystemPrompt
	}
	models := []ModelRef{{
		Provider: globalAI.Provider,
		APIkey:   globalAI.ApiKey,
		Model:    model,
	}}
	for _, fb := range agentCfg.Fallback {
		models = append(models, ModelRef{
			Provider: globalAI.Provider,
			APIkey:   globalAI.ApiKey,
			Model:    fb,
		})
	}
	return &Agent{
		id:           agentCfg.ID,
		systemPrompt: systemPrompt,
		models:       models,
		store:        store,
		abortReg:     abortReg,
		channelMgr:   chanMgr,
	}
}
