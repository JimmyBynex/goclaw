package main

import (
	"context"
	"goclaw/internal/agent"
	"goclaw/internal/channel"
	"goclaw/internal/config"
	"goclaw/internal/gateway"
	"goclaw/internal/memory"
	"goclaw/internal/session"
	"log"
	"os"
	"os/signal"
	"syscall"

	_ "goclaw/internal/ai/openrouter"
	_ "goclaw/internal/channel/telegram" // 触发 init() 注册
	_ "modernc.org/sqlite"
)

func main() {
	cfgMgr, err := config.NewManager("config.yaml")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	cfg := cfgMgr.Get()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go cfgMgr.Watch(ctx)

	store, err := session.NewFileStore(cfg.Session.Dir)
	if err != nil {
		log.Fatalf("[main] session.NewFileStore err: %v", err)
	}

	os.MkdirAll(cfg.Memory.Dir, 0755)
	memStore, err := memory.NewSQLiteStore(cfg.Memory.Dir + "/memory.db")
	if err != nil {
		log.Fatalf("[main] memory.NewSQLiteStore err: %v", err)
	}
	memoryMgr := memory.NewManager(memStore)

	// 1. 先建 Gateway
	gw := gateway.New(cfgMgr, store)

	// 2. 用 Gateway 的 InboundHandler 建 ChannelManager
	chanMgr := channel.NewManager(gw.InboundHandler())

	// 3. 把 ChannelManager 注入给 Gateway
	gw.SetChannelManager(chanMgr)

	// 4. 构建agentRgr
	agentRgr := agent.NewRegistry(cfgMgr, store, chanMgr, memoryMgr)

	gw.SetAgentRegistry(agentRgr)

	// 5. 启动 Telegram 渠道
	if err := chanMgr.Start(ctx, "telegram", cfg.Telegram.AccountId, map[string]any{
		"token": cfg.Telegram.Token,
	}); err != nil {
		log.Fatalf("[main] start telegram: %v", err)
	}

	// 6. 配置变更时重启受影响的渠道
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

	// 7. 启动 Gateway（阻塞）
	go func() {
		if err := gw.Start(ctx); err != nil {
			log.Printf("[main] gateway error: %v", err)
		}
	}()

	// 8. 等待退出
	<-ctx.Done()
	chanMgr.StopAll()
}
