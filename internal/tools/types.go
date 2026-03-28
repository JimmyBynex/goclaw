package tools

import (
	"context"
	"encoding/json"
)

// 描述可供ai调用的工具
type Tool struct {
	Name        string
	Description string //描述功能
	InputSchema map[string]any
	Execute     func(ctx context.Context, input json.RawMessage) (output string, err error)
	Policy      ToolPolicy
}

// ToolPolicy 描述工具的访问策略
type ToolPolicy struct {
	// RequireConfirmation：执行前需要用户确认（危险操作）
	RequireConfirmation bool
	// Sandbox：在沙箱中执行（代码执行类工具）
	Sandbox bool
	// AllowedAgents：空=所有 Agent 可用，非空=只有列表中的 Agent 可用
	AllowedAgents []string
}

type ToolUseBlock struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type ToolResultBlock struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error"`
}

// Response 是 AI 一次生成的完整响应
// 可能是纯文本，也可能包含工具调用
type Response struct {
	// StopReason 告诉我们为什么 AI 停止了
	// "end_turn" = 正常结束（纯文本回复）
	// "tool_use" = 需要调用工具
	StopReason string

	// Text 是纯文本内容（StopReason="end_turn" 时有值）
	Text string

	// ToolCalls 是 AI 要求调用的工具列表（StopReason="tool_use" 时有值）
	ToolCalls []ToolUseBlock
}
