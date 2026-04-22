package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"goclaw/internal/agent"
	"goclaw/internal/channel"
	"goclaw/internal/session"
	"log"
	"time"
)

type ChatHandler struct {
	store    session.Store
	hub      *Hub
	chanMgr  *channel.Manager
	agentReg *agent.Registry //我觉得职责相当于manager
}

func NewChatHandler(store session.Store, hub *Hub, chanMgr *channel.Manager, agentReg *agent.Registry) *ChatHandler {
	return &ChatHandler{
		store:    store,
		hub:      hub,
		chanMgr:  chanMgr,
		agentReg: agentReg,
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

	aborted := h.agentReg.Abort(params.RunID)
	return map[string]bool{"aborted": aborted}, nil
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
	ag, err := h.agentReg.Get(sess.Key.AgentID)
	if err != nil {
		log.Printf("[gateway]Failed to get agent: %v", err)
		return nil, err
	}

	//go通过通信共享数据，标准的示例解耦
	eventCh := make(chan agent.AgentEvent, 64)
	go func() {
		for e := range eventCh {
			h.hub.BroadCast(NewEvent(e.Type, e.Data))
		}
	}()

	go func() {
		defer close(eventCh)
		result, err := ag.RunReply(ctx, sess, params.Text, params.RunID, eventCh,
			sess.Key.ChannelID, sess.Key.AccountID, sess.Key.PeerID)
		if err != nil {
			return
		}
		//发回channel
		ch, err := h.chanMgr.Get(sess.Key.ChannelID, sess.Key.AccountID)
		if err == nil {
			ch.Send(ctx, channel.OutboundMessage{
				PeerID: sess.Key.PeerID,
				Text:   result.Reply,
			})
		}
	}()
	return SendResult{RunID: params.RunID, Status: "started"}, nil

}

func (h *ChatHandler) InboundHandler() channel.InBoundHandler {

	return func(ctx context.Context, msg channel.InBoundMessage) {
		scope := session.ScopeDM
		if msg.ChatType != "private" {
			scope = session.ScopeGroup
		}
		sessionKey := session.SessionKey{
			ChannelID: msg.ChannelID,
			AccountID: msg.AccountID,
			Scope:     scope,
			PeerID:    msg.PeerID,
			AgentID:   "default",
		}
		sess, err := h.store.Get(sessionKey)
		if err != nil {
			log.Printf("[gateway]Failed to get session: %v", err)
			return
		}
		//生成runID
		runID := fmt.Sprintf("%d", time.Now().UnixNano())
		ag, err := h.agentReg.Get("default")
		if err != nil {
			return
		}
		//和前文的逻辑一样
		eventCh := make(chan agent.AgentEvent, 64)
		go func() {
			for e := range eventCh {
				h.hub.BroadCast(NewEvent(e.Type, e.Data))
			}
		}()
		go func() {
			defer close(eventCh)
			result, err := ag.RunReply(ctx, sess, msg.Text, runID, eventCh,
				msg.ChannelID, msg.AccountID, msg.PeerID)
			log.Printf("[gateway] RunReply done: err=%v", err)
			if err != nil {
				return
			}
			log.Printf("[gateway] reply: %s", result.Reply)
			ch, err := h.chanMgr.Get(msg.ChannelID, msg.AccountID)
			if err == nil {
				ch.Send(ctx, channel.OutboundMessage{
					PeerID: msg.PeerID,
					Text:   result.Reply,
				})
			}
		}()
	}
}
