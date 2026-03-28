// internal/ai/client.go（修改）

package ai

import (
	"context"
	"goclaw/internal/tools"
)

// Client 是 AI 提供方的接口（修改版）
type Client interface {
	// Chat 发起一次对话，返回完整响应（含工具调用信息）
	// toolDefs 是可用工具的描述列表（nil 表示不使用工具）
	Chat(ctx context.Context, messages []Message, toolDefs []map[string]any) (*tools.Response, error)

	// StreamChat 流式对话（仅用于纯文本输出，工具调用走 Chat）
	// 注意：工具调用场景不支持流式，因为需要等完整响应才能执行工具
	StreamChat(ctx context.Context, messages []Message) (<-chan string, <-chan error)
}
