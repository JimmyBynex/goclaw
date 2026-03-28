// internal/tools/builtin/time.go

package builtin

import (
	"context"
	"encoding/json"
	"goclaw/internal/tools"
	"time"
)

// GetCurrentTimeTool 返回当前时间
// 这是最简单的工具示例，展示工具的基本结构
var GetCurrentTimeTool = &tools.Tool{
	Name:        "get_current_time",
	Description: "Get the current date and time. Use this when the user asks about the current time or date.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"timezone": map[string]any{
				"type":        "string",
				"description": "IANA timezone name, e.g. 'Asia/Shanghai'. Defaults to UTC.",
			},
		},
		"required": []string{},
	},
	Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
		var params struct {
			Timezone string `json:"timezone"`
		}
		json.Unmarshal(input, &params)

		loc := time.UTC
		if params.Timezone != "" {
			var err error
			loc, err = time.LoadLocation(params.Timezone)
			if err != nil {
				loc = time.UTC
			}
		}

		now := time.Now().In(loc)
		return now.Format("2006-01-02 15:04:05 MST"), nil
	},
}
