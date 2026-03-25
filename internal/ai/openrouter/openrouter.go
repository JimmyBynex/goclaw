package openrouter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"goclaw/internal/ai"
	"log"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	apiKey string
	model  string
	http   *http.Client
}

type msgPayload struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Messages []msgPayload `json:"messages"`
	Model    string       `json:"model"`
	Stream   bool         `json:"stream"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

func New(apiKey string, model string) *Client {
	return &Client{
		apiKey: apiKey,
		model:  model,
		http:   &http.Client{Timeout: 120 * time.Second},
	} //http.client相当于一个配置容器+连接池管理器
}

func (c *Client) StreamChat(ctx context.Context, message []ai.Message) (<-chan string, <-chan error) {
	textCh := make(chan string, 32)
	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		defer close(textCh)

		error := c.Stream(ctx, message, textCh)
		if error != nil {
			if ctx.Err() != nil {
				return
			}
			errCh <- error
		}
	}()
	return textCh, errCh
}

// chan<-只写，<-chan只读
func (c *Client) Stream(ctx context.Context, message []ai.Message, textch chan<- string) error {
	//组装playload
	playload := chatRequest{Model: c.model, Stream: true}
	for _, msg := range message {
		playload.Messages = append(playload.Messages, msgPayload{msg.Role, msg.Content})
	}

	//json化
	body, err := json.Marshal(playload)
	if err != nil {
		return err
	}

	//组装
	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://openrouter.ai/api/v1/chat/completions",
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("HTTP-Referer", "https://github.com/JimmyBynex/goclaw")
	req.Header.Set("X-Title", "goclaw")

	//实际请求
	resp, err := c.http.Do(req)
	if err != nil {
		log.Printf("[openrouter] http error: %v", err)
		return err
	}
	log.Printf("[openrouter] status: %d", resp.StatusCode)
	//请求出错
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var errBody struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&errBody)
		return fmt.Errorf("openrouter %d: %s", resp.StatusCode, errBody.Error.Message)
	}

	//成功返回，解析body
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	for scanner.Scan() {
		line := scanner.Text()

		//去除空行
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		//去除data：，留下json
		data := strings.TrimPrefix(line, "data:")

		//正常退出路径
		if data == "[DONE]" {
			return nil
		}

		//反json
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		content := chunk.Choices[0].Delta.Content
		log.Printf("[openrouter] chunk: %s", content)
		if content != "" {
			//防止收到ctx退出的时候，防止textch <- content卡住
			select {
			case textch <- content:
			case <-ctx.Done():
				return nil
			}
		}
	}
	return scanner.Err() //正常退出的路是上面遇到“[DONE]”

}

func init() {
	ai.RegisterModelFactory("openrouter", func(apiKey, model string) ai.Client { return New(apiKey, model) })
}
