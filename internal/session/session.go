package session

import (
	"goclaw/internal/ai"
	"time"
)

type Session struct {
	Key       SessionKey   `json:"key"`
	Messages  []ai.Message `json:"messages"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}

func New(k SessionKey) *Session {
	return &Session{
		Key:       k,
		Messages:  []ai.Message{},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now()}
}

func (s *Session) AddUserMessage(text string) {
	s.Messages = append(s.Messages, ai.Message{Role: "user", Content: text})
	s.UpdatedAt = time.Now()
}

func (s *Session) AddAssistantMessage(text string) {
	s.Messages = append(s.Messages, ai.Message{Role: "assistant", Content: text})
	s.UpdatedAt = time.Now()
}

func (s *Session) MessagesForAI(systemPrompt string, maxPairs int) []ai.Message {
	var result []ai.Message
	msgs := s.Messages
	if systemPrompt != "" {
		result = append(result, ai.Message{Role: "system", Content: systemPrompt})
	}
	n := len(s.Messages)
	if n < 2*maxPairs {
		result = append(result, msgs...)
	} else {
		msgs = msgs[n-2*maxPairs:]
		if msgs[0].Role != "user" {
			msgs = msgs[1:]
		}
		result = append(result, msgs...)
	}
	return result
}
