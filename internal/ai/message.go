package ai

import "goclaw/internal/tools"

// Message 扩展：支持工具结果类型
type Message struct {
	Role    string // "system" | "user" | "assistant" | "tool"
	Content string

	// 工具调用相关（Role="assistant" 时，AI 的工具调用请求）
	ToolCalls []tools.ToolUseBlock `json:",omitempty"`

	// 工具结果相关（Role="tool" 时，工具执行结果）
	ToolResults []tools.ToolResultBlock `json:",omitempty"`
}
