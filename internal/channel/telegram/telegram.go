package telegram

import (
	"context"
	"errors"
	"goclaw/internal/channel"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Bot struct {
	accountID string
	token     string
	apiBase   string
	client    *http.Client
	handler   channel.InBoundHandler
	status    channel.ChannelStatus
}

func New(accountID string, cfg map[string]any, handler channel.InBoundHandler) (*Bot, error) {
	token, _ := cfg["token"].(string)
	if token == "" {
		log.Printf("[telegram] no token provided]")
		return nil, errors.New("token required")
	}
	return &Bot{
		accountID: accountID,
		token:     cfg["token"].(string),
		apiBase:   "https://api.telegram.org/bot" + token,
		client:    &http.Client{Timeout: 35 * time.Second},
		handler:   handler,
	}, nil
}

func (b *Bot) Send(ctx context.Context, msg channel.OutboundMessage) (string, error) {
	result, err := b.sendMessage(msg.PeerID, msg.Text)
	if err != nil {
		log.Printf("[telegram] failed to send message: %v", err)
		return "", err
	}
	return strconv.Itoa(result.MessageID), nil
}

func (b *Bot) Start(ctx context.Context) error {
	log.Println("[telegram] starting telegram bot")
	offset := 0
	for {
		select {
		case <-ctx.Done():
			log.Println("[telegram] stopping telegram bot")
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
				//转换格式
				msg := channel.InBoundMessage{
					Raw:       update,
					ChannelID: "telegram",
					AccountID: b.accountID,
					PeerID:    strconv.FormatInt(update.Message.Chat.ID, 10),
					ChatType:  update.Message.Chat.Type,
					Text:      update.Message.Text,
					UserID:    strconv.FormatInt(update.Message.From.ID, 10), //具体发送者
				}
				go b.handler(ctx, msg)
			}
		}
	}
	return nil
}

func (b *Bot) SendStream(ctx context.Context, peerID string, textCh <-chan string) error {
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	messageID, err := b.Send(ctx, channel.OutboundMessage{Text: "...", PeerID: peerID, ParseMode: "Markdown"})
	if err != nil {
		log.Printf("[telegram] failed to send message: %v", err)
		return err
	}
	var fullReply strings.Builder
	var last string
	for {
		select {
		case <-ctx.Done():
			log.Println("[telegram] stopping sendstreaming")
			return nil
		case chunk, ok := <-textCh:
			if !ok {
				b.editMessage(ctx, peerID, messageID, fullReply.String())
				return nil
			}
			fullReply.WriteString(chunk)
		case <-ticker.C:
			text := fullReply.String()
			if last != text {
				b.editMessage(ctx, peerID, messageID, text)
				last = text
			}
		}

	}
}

func (b *Bot) Stop() error {
	return nil
}

func (b *Bot) Status() channel.ChannelStatus {
	return b.status
}

func (b *Bot) ID() string {
	return "telegram"
}

func (b *Bot) AccountID() string {
	return b.accountID
}

// init是特殊函数，被import的时候自动执行
func init() {
	channel.Register(
		"telegram", func(accountID string, cfg map[string]any, handler channel.InBoundHandler) (channel.Channel, error) {
			return New(accountID, cfg, handler)
		})
}

//工厂函数示例
// ChannelManager 需要创建时，从 registry 取出来调
//f := registry["telegram"]
//bot := f(accountID, cfg, handler)
