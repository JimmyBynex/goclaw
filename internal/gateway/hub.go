package gateway

import (
	"encoding/json"
	"log"
)

type Client struct {
	id   string
	send chan []byte
	hub  *Hub
}

type Hub struct {
	clients    map[*Client]bool
	broadcast  chan EventFrame
	register   chan *Client
	unregister chan *Client
}

func NewHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan EventFrame, 256),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[hub] client %s connected, total: %d", client.id, len(h.clients))

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
				log.Printf("[hub] client %s disconnected, total: %d", client.id, len(h.clients))
			}

		case event := <-h.broadcast:
			data, _ := json.Marshal(event)
			for client := range h.clients {
				select {
				case client.send <- data:
				// 防止其中一个客户端的send满了导致阻塞后续的客户端
				default:
					log.Printf("[hub] client %s send buffer full, dropping", client.id)
					delete(h.clients, client)
				}
			}
		}
	}
}

func (h *Hub) Register(client *Client) {
	h.register <- client
}
func (h *Hub) Unregister(client *Client) {
	h.unregister <- client
}

func (h *Hub) BroadCast(event EventFrame) {
	select {
	case h.broadcast <- event:
	default:
		log.Println("[hub] broadcast channel full, dropping event")
	}
}

func (h *Hub) ClientCount() int {
	return len(h.clients)
}
