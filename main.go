package main

import (
	"context"
	"fmt"
	"goclaw/internal/ai"
	"goclaw/internal/ai/openrouter"
	session "goclaw/internal/session"
	"goclaw/internal/telegram"
	"log"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Telegram struct {
		Token     string `yaml:"token"`
		AccountID string `yaml:"account_id"`
	} `yaml:"telegram"`
	AI struct {
		APIKey          string `yaml:"api_key"`
		Model           string `yaml:"model"`
		SystemPrompt    string `yaml:"system_prompt"`
		MaxContextPairs int    `yaml:"max_context_pairs"`
	} `yaml:"ai"`
	Session struct {
		Dir          string `yaml:"dir"`
		MaxIdleHours int    `yaml:"max_idle_hours"`
	}
}

func main() {
	data, _ := os.ReadFile("config.yaml")
	var cfg Config
	yaml.Unmarshal(data, &cfg)

	aiClient := openrouter.New(cfg.AI.APIKey, cfg.AI.Model)
	store, err := session.NewFileStore(cfg.Session.Dir)
	if err != nil {
		log.Printf("[main]session.NewFileStore err: %v", err)
	}
	ctx := context.Background()
	handler := makeHandler(aiClient, cfg, store)
	bot := telegram.New(cfg.Telegram.Token, handler)
	bot.StartPolling(ctx)
}

func makeHandler(aiClient ai.Client, cfg Config, store session.Store) telegram.MessageHandler {
	return func(ctx context.Context, msg *telegram.Message) (<-chan string, <-chan error) {
		scope := session.ScopeDM
		peerID := fmt.Sprintf("%d", msg.From.ID)
		if msg.Chat.Type != "private" {
			scope = session.ScopeGroup
		}
		key := session.SessionKey{
			AccountID: cfg.Telegram.AccountID,
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
