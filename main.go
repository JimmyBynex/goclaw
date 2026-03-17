package main

import (
	"context"
	"goclaw/internal/ai"
	"goclaw/internal/ai/openrouter"
	"goclaw/internal/telegram"
	"os"

	"gopkg.in/yaml.v3"
)

func main() {
	type Config struct {
		Telegram struct {
			Token string `yaml:"token"`
		} `yaml:"telegram"`
		AI struct {
			APIKey string `yaml:"api_key"`
			Model  string `yaml:"model"`
		} `yaml:"ai"`
	}
	data, _ := os.ReadFile("config.yaml")
	var cfg Config
	yaml.Unmarshal(data, &cfg)

	aiClient := openrouter.New(cfg.AI.APIKey, cfg.AI.Model)
	ctx := context.Background()
	handler := func(ctx context.Context, msg *telegram.Message) (<-chan string, <-chan error) {
		messages := []ai.Message{
			{Role: "user", Content: msg.Text},
		}
		return aiClient.StreamChat(ctx, messages)
	}
	bot := telegram.New(cfg.Telegram.Token, handler)
	bot.StartPolling(ctx)
}
