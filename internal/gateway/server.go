package gateway

import (
	"context"
	"fmt"
	"goclaw/internal/ai"
	"goclaw/internal/session"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

type Config struct {
	Port         int
	Token        string
	SystemPrompt string
	MaxPairs     int
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Gateway struct {
	hub    *Hub    //管理链接
	router *Router //如何处理链接的数据
	auth   *Auth   //鉴权
	server *http.Server
}

func (g *Gateway) ServerWS(w http.ResponseWriter, r *http.Request) {
	//1.先auth鉴权认证
	if !g.auth.Validate(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	//2.再升级为websocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[gateway]upgrader.Upgrade failed:", err)
		return
	}

	//注册
	send := make(chan []byte, 256)
	id := fmt.Sprintf("%d", time.Now().UnixNano()) //时间戳
	client := &Client{id: id, send: send, hub: g.hub}
	g.hub.Register(client)

	//启动
	ctx := context.Background()
	go client.readPump(conn, ctx, g.router)
	go client.writePump(conn)
}

func New(cfg Config, aiClient ai.Client, store session.Store) *Gateway {
	hub := NewHub()
	router := NewRouter()

	health := NewHealthHandler()
	router.Register("health", health.Health)

	chat := NewChatHandler(cfg.MaxPairs, aiClient, store, hub, cfg.SystemPrompt)
	router.Register("chat.send", chat.Send)
	router.Register("chat.history", chat.History)
	router.Register("chat.abort", chat.Abort)
	g := &Gateway{
		hub:    hub,
		router: router,
		auth:   NewAuth(cfg.Token),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", g.ServerWS)
	g.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: mux,
	}
	return g
}

func (g *Gateway) Start(ctx context.Context) error {
	go g.hub.Run()
	go func() {
		<-ctx.Done()
		g.server.Shutdown(context.Background())
	}()
	if err := g.server.ListenAndServe(); err !=
		http.ErrServerClosed {
		return err
	}
	return nil
}

func (g *Gateway) ServeHealthHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"clients":%d}`, g.hub.ClientCount())
}
