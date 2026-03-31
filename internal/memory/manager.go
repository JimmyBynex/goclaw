package memory

import (
	"context"
	"fmt"
	"goclaw/internal/ai"
	"log"
	"strings"
)

type Manager struct {
	store     Store
	extractor *ai.Client
}

func NewManager(store Store) *Manager {
	return &Manager{store: store}
}

func (m *Manager) InjectMemories(
	ctx context.Context,
	agentID string,
	userInput string,
	messages []ai.Message,
) []ai.Message {
	//1.构造搜索块进行检索
	entries, err := m.store.Search(SearchQuery{
		AgentID: agentID,
		Query:   userInput,
		Limit:   5,
	})
	if err != nil || len(entries) == 0 {
		log.Printf("[memory]not finding related memories %v", err)
		return messages
	}

	// 构造记忆块文本
	var memBlock strings.Builder
	memBlock.WriteString("\n\n--- Relevant memories ---\n")
	for i, e := range entries {
		memBlock.WriteString(fmt.Sprintf("%d. %s\n", i+1, e.Content))
	}
	memBlock.WriteString("--- End of memories ---")

	//注入到system message
	injected := make([]ai.Message, len(messages))
	copy(injected, messages)
	for i, msg := range injected {
		if msg.Role == "system" {
			//一定要这样才能改原切片，否则就只是改副本
			injected[i].Content = msg.Content + memBlock.String()
			return injected
		}
	}

	// 没有 system message，在开头插入一条
	return append([]ai.Message{
		{Role: "system", Content: "You are a helpful assistant." + memBlock.String()},
	}, injected...)
}

// ExtractAndSave 在 AI 回复后，提取值得记住的信息
// 简单版本：规则提取（不调用额外的 AI）
// 高级版本：调用 AI 分析对话，提取结构化记忆
func (m *Manager) ExtractAndSave(ctx context.Context, agentID, sessionID, userInput, aiReply string) {
	entries := m.ruleBasedExtract(agentID, sessionID, userInput, aiReply)
	for _, e := range entries {
		if err := m.store.Save(e); err != nil {
			log.Printf("[memory] save failed: %v", err)
		}
	}
}

// ruleBasedExtract 用规则提取值得记忆的信息
// 规则：用户自我介绍、偏好声明、重要决定等
func (m *Manager) ruleBasedExtract(agentID, sessionID, userInput, aiReply string) []*Entry {
	var entries []*Entry
	lower := strings.ToLower(userInput)

	// 规则 1：用户说"我是..."（身份信息）
	if containsAny(lower, "我是", "我叫", "my name is", "i am a") {
		entries = append(entries, &Entry{
			AgentID:   agentID,
			SessionID: sessionID,
			Content:   userInput,
			Tags:      []string{"identity"},
			Source:    "user_message",
		})
	}

	// 规则 2：用户说"我喜欢/不喜欢..."（偏好）
	if containsAny(lower, "我喜欢", "我不喜欢", "i like", "i prefer", "i hate") {
		entries = append(entries, &Entry{
			AgentID:   agentID,
			SessionID: sessionID,
			Content:   userInput,
			Tags:      []string{"preference"},
			Source:    "user_message",
		})
	}

	// 规则 3：用户说"记住..."（明确要求记忆）
	if containsAny(lower, "记住", "请记住", "remember that", "note that") {
		entries = append(entries, &Entry{
			AgentID:   agentID,
			SessionID: sessionID,
			Content:   strings.TrimPrefix(userInput, "记住"),
			Tags:      []string{"explicit", "important"},
			Source:    "user_message",
		})
	}

	return entries
}

func containsAny(s string, keywords ...string) bool {
	for _, keyword := range keywords {
		if strings.Contains(s, keyword) {
			return true
		}
	}
	return false
}
