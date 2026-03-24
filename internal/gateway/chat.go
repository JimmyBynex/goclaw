package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"goclaw/internal/ai"
	"goclaw/internal/channel"
	"goclaw/internal/config"
	"goclaw/internal/session"
	"log"
	"strings"
	"sync"
	"time"
)

type ChatHandler struct {
	aiClient ai.Client
	store    session.Store
	hub      *Hub
	mu       sync.Mutex
	cancels  map[string]context.CancelFunc
	cfgMgr   *config.Manager
	chanMgr  *channel.Manager
}

func NewChatHandler(aiClient ai.Client, store session.Store, hub *Hub, cfgMgr *config.Manager, chanMgr *channel.Manager) *ChatHandler {
	return &ChatHandler{
		aiClient: aiClient,
		store:    store,
		hub:      hub,
		cancels:  make(map[string]context.CancelFunc),
		cfgMgr:   cfgMgr,
		chanMgr:  chanMgr,
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
		h.handleChat(ctx, sess, params.Text, params.RunID, nil)

	}()
	return SendResult{RunID: params.RunID, Status: "started"}, nil

}

func (h *ChatHandler) handleChat(ctx context.Context, sess *session.Session, text, runID string, inbound *channel.InBoundMessage) {
	// AI调用、广播、发回Channel
	cfg := h.cfgMgr.Get()
	sess.AddUserMessage(text)
	messages := sess.MessagesForAI(cfg.AI.SystemPrompt, cfg.AI.MaxContextPairs)
	log.Printf("[chat] messages count: %d", len(messages))
	rawCh, errCh := h.aiClient.StreamChat(ctx, messages)

	var fullReply strings.Builder
	// 1. 先判断 inbound，启动 SendStream
	var textCh chan string
	if inbound != nil {
		ch, err := h.chanMgr.Get(inbound.ChannelID,
			inbound.AccountID)
		if err == nil {
			//类型判断
			if ss, ok := ch.(channel.StreamSender); ok {
				textCh = make(chan string, 32)
				go ss.SendStream(ctx, inbound.PeerID, textCh)
			}
		}
	}

	// 2. 再读流
	for chunk := range rawCh {
		h.hub.BroadCast(NewEvent("chat.delta",
			map[string]string{
				"run_id": runID,
				"chunk":  chunk,
			}))
		fullReply.WriteString(chunk)
		if textCh != nil {
			textCh <- chunk
		}
	}
	if textCh != nil {
		close(textCh)
	}
	log.Printf("[chat] rawCh closed, checking errCh")
	if err := <-errCh; err != nil {
		//主动推错误信号
		log.Printf("[chat] errCh error: %v", err)
		h.hub.BroadCast(NewEvent("chat.error",
			map[string]string{
				"run_id":  runID,
				"message": err.Error(),
			}))
		return
	}

	reply := fullReply.String()
	sess.AddAssistantMessage(reply)
	h.store.Save(sess)
	h.hub.BroadCast(NewEvent("chat.done", map[string]string{
		"run_id": runID,
		"text":   reply,
	}))
}

func (h *ChatHandler) InboundHandler() channel.InBoundHandler {
	return func(ctx context.Context, msg channel.InBoundMessage) {
		// 1. 构建 SessionKey
		scope := session.ScopeDM
		if msg.ChatType != "private" {
			scope = session.ScopeGroup
		}
		key := session.SessionKey{
			ChannelID: msg.ChannelID,
			AccountID: msg.AccountID,
			PeerID:    msg.PeerID,
			Scope:     scope,
			AgentID:   "default",
		}

		// 2. 取 Session
		sess, err := h.store.Get(key)
		if err != nil {
			log.Printf("[chat] get session failed: %v", err)
			return
		}

		// 3. 生成 runID，调 handleChat
		runID := fmt.Sprintf("%d", time.Now().UnixNano())
		h.handleChat(ctx, sess, msg.Text, runID, &msg)
	}
}
