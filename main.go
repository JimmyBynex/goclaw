package main

import (
	"context"
	"fmt"
	"goclaw/internal/ai"
	"goclaw/internal/ai/openrouter"
	"goclaw/internal/config"
	"goclaw/internal/gateway"
	session "goclaw/internal/session"
	"goclaw/internal/telegram"
	"log"
	"strings"
)

func main() {
	cfgMgr, err := config.NewManager("config.yaml")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	cfg := cfgMgr.Get()
	ctx := context.Background()

	cfgMgr.OnChange(func(old, new *config.Config) {
		switch config.Diff(old, new) {
		case config.ReloadNone:
			log.Println("[config] hot update applied")
		case config.ReloadChannel:
			log.Println("[config] telegram config changed, restart required")
		case config.ReloadGateway:
			log.Println("[config] gateway config changed, restart required")
		}
	})

	go cfgMgr.Watch(ctx)

	aiClient := openrouter.New(cfg.AI.ApiKey, cfg.AI.Model)
	store, err := session.NewFileStore(cfg.Session.Dir)
	if err != nil {
		log.Printf("[main]session.NewFileStore err: %v", err)
	}

	// 启动 Gateway
	gw := gateway.New(gateway.Config{
		Port:         cfg.Gateway.Port,
		Token:        cfg.Gateway.Token,
		SystemPrompt: cfg.AI.SystemPrompt,
		MaxPairs:     cfg.AI.MaxContextPairs,
	}, aiClient, store)
	go func() {
		if err := gw.Start(ctx); err != nil {
			log.Printf("[main] gateway error: %v", err)
		}
	}()

	handler := makeHandler(aiClient, cfgMgr, store)
	bot := telegram.New(cfg.Telegram.Token, handler)
	bot.StartPolling(ctx)
}

func makeHandler(aiClient ai.Client, cfgMgr *config.Manager, store session.Store) telegram.MessageHandler {
	return func(ctx context.Context, msg *telegram.Message) (<-chan string, <-chan error) {
		cfg := cfgMgr.Get()
		scope := session.ScopeDM
		peerID := fmt.Sprintf("%d", msg.From.ID)
		if msg.Chat.Type != "private" {
			scope = session.ScopeGroup
		}
		key := session.SessionKey{
			AccountID: cfg.Telegram.AccountId,
			ChannelID: "telegram",
			Scope:     scope,
			PeerID:    peerID,
			AgentID:   "default",
		}

		sess, err := store.Get(key)
		if err != nil {
			log.Printf("[main]:Get session fail: %v", err)
		}
		sess.AddUserMessage(msg.Text)
		messages := sess.MessagesForAI(cfg.AI.SystemPrompt, cfg.AI.MaxContextPairs)

		rawTextCh, errCh := aiClient.StreamChat(ctx, messages)

		//边消费边转发，利用新流(复制一份的感觉)
		textCh := make(chan string, 32)
		go func() {
			defer close(textCh)
			var fullReply strings.Builder
			for chunk := range rawTextCh {
				fullReply.WriteString(chunk)
				textCh <- chunk // 转发
			}
			// 流结束才保存
			sess.AddAssistantMessage(fullReply.String())
			store.Save(sess)
		}()

		return textCh, errCh
	}

}
