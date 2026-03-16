package telegram

import (
	"context"
	"log"
	"net/http"
	"time"
)

type MessageHandler func(ctx context.Context, msg *Message) (<-chan string, <-chan error)

type Bot struct {
	token   string
	apiBase string
	client  *http.Client
	handler MessageHandler
}

func New(token string, handler MessageHandler) *Bot {
	return &Bot{
		token:   token,
		apiBase: "https://api.telegram.org/bot" + token,
		client:  &http.Client{Timeout: time.Second * 60},
		handler: handler,
	}
}

func (b *Bot) StartPolling(ctx context.Context) error {
	log.Println("[telegram]:starting polling")
	offset := 0
	for {
		select {
		case <-ctx.Done():
			log.Println("[telegram] polling stopped")
			return nil
		default:
		}

		updates, err := b.getUpdates(ctx, offset, 30)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("[telegram] getUpdates error: %v, retrying...", err)
			time.Sleep(3 * time.Second)
			continue
		}
		for _, update := range updates {
			offset = update.UpdateId + 1

			if update.Message != nil {
				go b.handleMessage(ctx, update.Message)
			}

		}
	}
}

func (b *Bot) handleMessage(ctx context.Context, msg *Message) {
	log.Printf("[telegram]:handling message from %d: %s\n", msg.Chat.ID, msg.Text)
	placeholder, err := b.sendMessage(msg.Chat.ID, "...")
	if err != nil {
		log.Printf("[telegram] send placeholder failed: %v", err)
		return
	}
	textCh, errCh := b.handler(ctx, msg)
	b.streamToTelegram(ctx, msg.Chat.ID, placeholder.MessageID, textCh, errCh)
}
