package gateway

import (
	"context"
	"encoding/json"
	"goclaw/internal/ai"
	"goclaw/internal/session"
	"log"
	"strings"
	"sync"
)

type ChatHandler struct {
	aiClient     ai.Client
	store        session.Store
	hub          *Hub
	systemPrompt string
	maxPairs     int
	mu           sync.Mutex
	cancels      map[string]context.CancelFunc
}

func NewChatHandler(maxPaires int, aiClient ai.Client, store session.Store, hub *Hub, systemPrompt string) *ChatHandler {
	return &ChatHandler{
		aiClient:     aiClient,
		store:        store,
		hub:          hub,
		systemPrompt: systemPrompt,
		maxPairs:     maxPaires,
		cancels:      make(map[string]context.CancelFunc),
	}
}

type HistoryParams struct {
	SessionKey string `json:"session_key"`
}

func (c *ChatHandler) History(ctx context.Context, raw json.RawMessage) (any, error) {
	var params HistoryParams
	json.Unmarshal(raw, &params)
	sessionKey, err := session.Parse(params.SessionKey)
	if err != nil {
		log.Printf("[gateway]Failed to parse session key: %v", err)
		return nil, err
	}
	sess, err := c.store.Get(sessionKey)
	if err != nil {
		log.Printf("[gateway]Failed to get session: %v", err)
		return nil, err
	}
	//只需要[]ai.Messages
	return sess.Messages, nil
}

type AbortParams struct {
	RunID string `json:"run_id"`
}

func (h *ChatHandler) Abort(ctx context.Context, raw json.RawMessage) (any, error) {
	var params AbortParams
	json.Unmarshal(raw, &params)

	h.mu.Lock()
	cancel, ok := h.cancels[params.RunID]
	h.mu.Unlock()

	if ok {
		cancel()
	}
	return map[string]bool{"aborted": ok}, nil
}

type SendParams struct {
	SessionKey string `json:"session_key"`
	Text       string `json:"text"`
	RunID      string `json:"run_id"`
}

type SendResult struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

func (h *ChatHandler) Send(ctx context.Context, raw json.RawMessage) (any, error) {
	var params SendParams
	json.Unmarshal(raw, &params)
	sessionKey, err := session.Parse(params.SessionKey)
	if err != nil {
		log.Printf("[gateway]Failed to parse session key: %v", err)
		return nil, err
	}
	sess, err := h.store.Get(sessionKey)
	if err != nil {
		log.Printf("[gateway]Failed to get session: %v", err)
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)

	h.mu.Lock()
	h.cancels[params.RunID] = cancel
	h.mu.Unlock()

	go func() {

		defer func() {
			cancel()
			h.mu.Lock()
			delete(h.cancels, params.RunID)
			h.mu.Unlock()
		}()

		sess.AddUserMessage(params.Text)
		messages := sess.MessagesForAI(h.systemPrompt, h.maxPairs)
		log.Printf("[chat] messages count: %d", len(messages))
		rawCh, errCh := h.aiClient.StreamChat(ctx, messages)

		var fullReply strings.Builder

		for chunk := range rawCh {
			log.Printf("[chat] chunk: %q", chunk)
			h.hub.BroadCast(NewEvent("chat.delta", map[string]string{
				"run_id": params.RunID,
				"chunk":  chunk,
			}))
			fullReply.WriteString(chunk)
		}
		log.Printf("[chat] rawCh closed, checking errCh")
		if err := <-errCh; err != nil {
			//主动推错误信号
			log.Printf("[chat] errCh error: %v", err)
			h.hub.BroadCast(NewEvent("chat.error",
				map[string]string{
					"run_id":  params.RunID,
					"message": err.Error(),
				}))
			return
		}

		reply := fullReply.String()
		sess.AddAssistantMessage(reply)
		h.store.Save(sess)
		h.hub.BroadCast(NewEvent("chat.done", map[string]string{
			"run_id": params.RunID,
			"text":   reply,
		}))

	}()
	return SendResult{RunID: params.RunID, Status: "started"}, nil

}
