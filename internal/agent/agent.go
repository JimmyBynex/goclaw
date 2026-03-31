package agent

import (
	"goclaw/internal/channel"
	"goclaw/internal/config"
	"goclaw/internal/memory"
	"goclaw/internal/session"
	"goclaw/internal/tools"
	"goclaw/internal/tools/builtin"
	"time"
)

// AI代理的核心
// 对话代理的实例，包括用什么模型，提示词，存储，中止
type Agent struct {
	id           string
	systemPrompt string
	models       []ModelRef
	store        session.Store //接口，已经是引用
	abortReg     *AbortRegistry
	toolRegistry *tools.Registry
	executor     *tools.Executor
	channelMgr   *channel.Manager //推理期间发 typing indicator
	memoryMgr    *memory.Manager
}
type ModelRef struct {
	Provider string
	APIKey   string
	Model    string
}

func FromConfig(
	agentCfg config.AgentConfig,
	globalAI config.AIConfig,
	store session.Store,
	abortReg *AbortRegistry,
	chanMgr *channel.Manager,
	memoryMgr *memory.Manager,
) *Agent {
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
		APIKey:   globalAI.ApiKey,
		Model:    model,
	}}
	for _, fb := range agentCfg.Fallback {
		models = append(models, ModelRef{
			Provider: globalAI.Provider,
			APIKey:   globalAI.ApiKey,
			Model:    fb,
		})
	}
	//在这里注册工具了
	registry := setupTools()
	executor := tools.NewExecutor(registry, 30*time.Second)
	return &Agent{
		id:           agentCfg.ID,
		systemPrompt: systemPrompt,
		models:       models,
		store:        store,
		abortReg:     abortReg,
		channelMgr:   chanMgr,
		toolRegistry: registry,
		executor:     executor,
		memoryMgr:    memoryMgr,
	}
}

func setupTools() *tools.Registry {
	reg := tools.NewRegistry()

	reg.Register(builtin.GetCurrentTimeTool)
	reg.Register(builtin.CalculateTool)
	reg.Register(builtin.HTTPFetchTool)

	return reg
}
