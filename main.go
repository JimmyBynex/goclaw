package main

import (
	"context"
	"goclaw/internal/ai/openrouter"
	"goclaw/internal/channel"
	"goclaw/internal/config"
	"goclaw/internal/gateway"
	"goclaw/internal/session"
	"log"

	_ "goclaw/internal/ai/openrouter"
	_ "goclaw/internal/channel/telegram" // 触发 init() 注册
)

func main() {
	cfgMgr, err := config.NewManager("config.yaml")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	cfg := cfgMgr.Get()
	ctx := context.Background()

	go cfgMgr.Watch(ctx)

	aiClient := openrouter.New(cfg.AI.ApiKey, cfg.AI.Model)
	store, err := session.NewFileStore(cfg.Session.Dir)
	if err != nil {
		log.Fatalf("[main] session.NewFileStore err: %v", err)
	}

	// 1. 先建 Gateway
	gw := gateway.New(cfgMgr, aiClient, store)

	// 2. 用 Gateway 的 InboundHandler 建 ChannelManager
	chanMgr := channel.NewManager(gw.InboundHandler())

	// 3. 把 ChannelManager 注入给 Gateway
	gw.SetChannelManager(chanMgr)

	// 4. 启动 Telegram 渠道
	if err := chanMgr.Start(ctx, "telegram", cfg.Telegram.AccountId, map[string]any{
		"token": cfg.Telegram.Token,
	}); err != nil {
		log.Fatalf("[main] start telegram: %v", err)
	}

	// 5. 配置变更时重启受影响的渠道
	cfgMgr.OnChange(func(old, new *config.Config) {
		switch config.Diff(old, new) {
		case config.ReloadNone:
			log.Println("[config] hot update applied")
		case config.ReloadChannel:
			chanMgr.Stop("telegram", old.Telegram.AccountId)
			chanMgr.Start(ctx, "telegram", new.Telegram.AccountId, map[string]any{
				"token": new.Telegram.Token,
			})
		case config.ReloadGateway:
			log.Println("[config] gateway port changed, restart required")
		}
	})

	// 6. 启动 Gateway（阻塞）
	go func() {
		if err := gw.Start(ctx); err != nil {
			log.Printf("[main] gateway error: %v", err)
		}
	}()

	// 7. 等待退出
	<-ctx.Done()
	chanMgr.StopAll()
}
