package gateway

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

// bot收到消息 这是gateway发送给bot的
func (c *Client) writePump(conn *websocket.Conn) {
	ticker := time.NewTicker(54 * time.Second)
	defer func() {
		ticker.Stop()
		conn.Close()
	}()
	for {
		select {
		case <-ticker.C:
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			err3 := conn.WriteMessage(websocket.PingMessage, nil)
			if err3 != nil {
				log.Println("[gateway]write ping failed:", err3)
			}
		case msg, ok := <-c.send:
			if ok == false {

				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				err1 := conn.WriteMessage(websocket.CloseMessage, []byte{})
				if err1 != nil {
					log.Printf("[gateway]write pump error: %v", err1)
				}
				return
			}

			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			err2 := conn.WriteMessage(websocket.TextMessage, msg)
			if err2 != nil {
				log.Println("[gateway]write pump:", err2)
				return
			}

		}
	}
}

// bot发消息 这是bot发送给gateway
func (c *Client) readPump(conn *websocket.Conn, ctx context.Context, router *Router) {
	conn.SetReadLimit(512 * 1024)
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	defer conn.Close()
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Println("[gateway]read pump error:", err)
			return
		}
		var frame RequestFrame
		err = json.Unmarshal(msg, &frame)
		if err != nil {
			log.Println("[gateway]read pump error:", err)
			return
		}

		responseFrame := router.Dispatch(ctx, frame)
		data, _ := json.Marshal(responseFrame)
		c.send <- data
	}
}
