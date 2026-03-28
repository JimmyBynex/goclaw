// internal/tools/builtin/http_fetch.go

package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"goclaw/internal/tools"
)

// HTTPFetchTool 获取网页内容（纯文本）
// 这是一个有一定风险的工具：需要网络访问
// 生产环境应该加 URL 白名单过滤
var HTTPFetchTool = &tools.Tool{
	Name:        "http_fetch",
	Description: "Fetch the content of a URL and return it as plain text. Use for retrieving web pages, APIs, or public data.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "The URL to fetch",
			},
			"max_chars": map[string]any{
				"type":        "integer",
				"description": "Maximum characters to return (default 5000)",
			},
		},
		"required": []string{"url"},
	},
	Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
		var params struct {
			URL      string `json:"url"`
			MaxChars int    `json:"max_chars"`
		}
		if err := json.Unmarshal(input, &params); err != nil {
			return "", err
		}
		if params.MaxChars <= 0 {
			params.MaxChars = 5000
		}

		client := &http.Client{Timeout: 10 * time.Second}
		req, err := http.NewRequestWithContext(ctx, "GET", params.URL, nil)
		if err != nil {
			return "", fmt.Errorf("invalid URL: %w", err)
		}
		req.Header.Set("User-Agent", "goclaw/1.0")

		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("fetch failed: %w", err)
		}
		defer resp.Body.Close()

		// 限制读取大小，防止读取超大响应
		body, err := io.ReadAll(io.LimitReader(resp.Body, int64(params.MaxChars*4)))
		if err != nil {
			return "", err
		}

		// 去除 HTML 标签（简单版本）
		text := stripHTMLTags(string(body))
		if len(text) > params.MaxChars {
			text = text[:params.MaxChars] + "\n...(truncated)"
		}

		return fmt.Sprintf("HTTP %d\n\n%s", resp.StatusCode, text), nil
	},
}

func stripHTMLTags(html string) string {
	var sb strings.Builder
	inTag := false
	for _, r := range html {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
			sb.WriteRune(' ')
		case !inTag:
			sb.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(sb.String()), " ")
}
