package openrouter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"goclaw/internal/ai"
	"goclaw/internal/tools"
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
type toolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串
}

type toolCallItem struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // 固定 "function"
	Function toolCallFunction `json:"function"`
}

type msgPayload struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"` // 指针，允许 null
	ToolCalls  []toolCallItem `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type toolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type toolParam struct {
	Type     string       `json:"type"` // 固定 "function"
	Function toolFunction `json:"function"`
}

type chatRequest struct {
	Model    string       `json:"model"`
	Stream   bool         `json:"stream"`
	Messages []msgPayload `json:"messages"`
	Tools    []toolParam  `json:"tools,omitempty"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

type chatResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   *string        `json:"content"`
			ToolCalls []toolCallItem `json:"tool_calls,omitempty"`
		} `json:"message"`
	} `json:"choices"`
}

func New(apiKey string, model string) *Client {
	return &Client{
		apiKey: apiKey,
		model:  model,
		http:   &http.Client{Timeout: 120 * time.Second},
	} //http.client相当于一个配置容器+连接池管理器
}

func (c *Client) Chat(ctx context.Context, messages []ai.Message, toolDefs []map[string]any) (*tools.Response, error) {
	payload := chatRequest{Model: c.model, Stream: false}

	for _, msg := range messages {
		switch msg.Role {
		case "user", "system":
			content := msg.Content
			payload.Messages = append(payload.Messages, msgPayload{
				Role:    msg.Role,
				Content: &content,
			})
		case "assistant":
			if len(msg.ToolCalls) == 0 {
				// 普通 assistant 消息
				content := msg.Content
				payload.Messages = append(payload.Messages, msgPayload{
					Role:    msg.Role,
					Content: &content,
				})
			} else if len(msg.ToolCalls) > 0 {
				toolCalls := make([]toolCallItem, 0, len(msg.ToolCalls))
				for _, call := range msg.ToolCalls {
					toolCalls = append(toolCalls, toolCallItem{
						ID:   call.ID,
						Type: "function",
						Function: toolCallFunction{
							Name:      call.Name,
							Arguments: string(call.Input),
						},
					})
				}
				payload.Messages = append(payload.Messages, msgPayload{
					Role:      msg.Role,
					ToolCalls: toolCalls,
					Content:   nil,
				})

			}
		case "tool":
			// 每个 ToolResult 展开成一条消息
			for _, call := range msg.ToolResults {
				payload.Messages = append(payload.Messages, msgPayload{
					Role:       msg.Role,
					Content:    &call.Content,
					ToolCallID: call.ToolUseID,
				})
			}

		}
	}

	//最后每次添加工具
	for _, def := range toolDefs {
		payload.Tools = append(payload.Tools, toolParam{
			Type: "function",
			Function: toolFunction{
				Name:        def["name"].(string),
				Description: def["description"].(string),
				Parameters:  def["input_schema"].(map[string]any),
			}})
	}
	//json化
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	//组装
	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://openrouter.ai/api/v1/chat/completions",
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("HTTP-Referer", "https://github.com/JimmyBynex/goclaw")
	req.Header.Set("X-Title", "goclaw")

	//实际请求
	resp, err := c.http.Do(req)
	if err != nil {
		log.Printf("[openrouter] http error: %v", err)
		return nil, err
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
		return nil, fmt.Errorf("openrouter %d: %s", resp.StatusCode, errBody.Error.Message)
	}
	var result chatResponse
	json.NewDecoder(resp.Body).Decode(&result) //go自动映射字段
	if len(result.Choices) == 0 {
		return nil, fmt.Errorf("[ai]empty response")
	}

	choice := result.Choices[0]
	//回显分类
	switch choice.FinishReason {
	case "stop":
		var text string
		if choice.Message.Content != nil {
			text = *choice.Message.Content
		}
		return &tools.Response{
			StopReason: "end_turn",
			Text:       text,
		}, nil
	case "tool_calls":
		toolCalls := make([]tools.ToolUseBlock, 0, len(choice.Message.ToolCalls))
		for _, call := range choice.Message.ToolCalls {
			toolCalls = append(toolCalls, tools.ToolUseBlock{
				ID:    call.ID,
				Name:  call.Function.Name,
				Input: json.RawMessage(call.Function.Arguments),
			})
		}
		return &tools.Response{
			StopReason: "tool_use",
			ToolCalls:  toolCalls,
		}, nil
	default:
		return nil, fmt.Errorf("[ai]unknown choice reason: %s", choice.FinishReason)
	}
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
	//在go内，for range 只是把内容赋进msg,如果每次都只是&msg.content,那其实都是同一个地址
	for _, msg := range message {
		content := msg.Content
		playload.Messages = append(playload.Messages, msgPayload{Role: msg.Role, Content: &content})
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
