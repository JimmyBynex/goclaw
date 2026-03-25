package telegram

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (b *Bot) getUpdates(ctx context.Context, offset int, timeout int) ([]Update, error) {
	params := url.Values{}
	params.Set("offset", strconv.Itoa(offset))
	params.Set("timeout", strconv.Itoa(timeout))
	url := b.apiBase + "/getUpdates?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(req)

	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result getUpdatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Result, nil
}

// 为什么需要返回*Message，因为需要拿到message_id方便后续修改
func (b *Bot) sendMessage(ctx context.Context, peerID string, text string) (*Message, error) {

	params := url.Values{
		"chat_id":    {peerID},
		"text":       {text},
		"parse_mode": {"Markdown"},
	}
	url := b.apiBase + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(params.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result sendMessageResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result.Result, nil
}

func (b *Bot) editMessage(ctx context.Context, peerID string, messageID string, text string) error {
	params := url.Values{
		"chat_id":    {peerID},
		"message_id": {messageID},
		"text":       {text},
		"parse_mode": {"Markdown"},
	}
	url := b.apiBase + "/editMessageText"
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}
func (b *Bot) streamToTelegram(ctx context.Context, peerID string, messageID string, textCh <-chan string, errCh <-chan error) {
	var buf strings.Builder
	ticker := time.NewTicker(600 * time.Millisecond)
	defer ticker.Stop()

	lastSent, current := "", ""
	for {
		select {
		case chunk, ok := <-textCh:
			// ok==false 说明 channel 关闭了,最后发送
			if !ok {
				b.editMessage(ctx, peerID, messageID, buf.String())
				return
			}
			buf.WriteString(chunk)
			current = buf.String()
		case err := <-errCh:
			if err != nil {
				log.Printf("[Telegram] channel error: %v", err)
				return
			}
			errCh = nil

		case <-ticker.C:
			//防止出现没有更新的情况无效edit
			if current != lastSent {
				b.editMessage(ctx, peerID, messageID, current)
				lastSent = current
			}

		case <-ctx.Done():
			return
		}
	}
}
