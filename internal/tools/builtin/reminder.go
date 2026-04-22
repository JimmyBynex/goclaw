package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"goclaw/internal/cron"
	"goclaw/internal/tools"
)

func RegisterReminderTools(registry *tools.Registry, store *cron.Store, agentID, channelID, accountID, peerID string) {
	registry.Register(&tools.Tool{
		Name:        "create_reminder",
		Description: "创建一个定时提醒。一次性提醒传 RFC3339 时间（如 2026-04-23T09:00:00+08:00）或常见格式（如 2026-04-23 09:00），repeat 不填或填 false。重复提醒传 cron 表达式（如 30 8 * * 1 表示每周一8:30），repeat 填 true。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message":  map[string]any{"type": "string", "description": "提醒内容"},
				"schedule": map[string]any{"type": "string", "description": "触发时间或 cron 表达式"},
				"repeat":   map[string]any{"type": "boolean", "description": "是否重复，true=按 cron 重复，false=只触发一次"},
			},
			"required": []string{"message", "schedule"},
		},
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				Message  string `json:"message"`
				Schedule string `json:"schedule"`
				Repeat   bool   `json:"repeat"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", err
			}
			nextRun, err := cron.ParseNextRun(p.Schedule, p.Repeat, time.Now())
			if err != nil {
				return "", fmt.Errorf("无法解析时间: %w", err)
			}
			job := &cron.Job{
				AgentID:   agentID,
				ChannelID: channelID,
				AccountID: accountID,
				PeerID:    peerID,
				Message:   p.Message,
				Schedule:  p.Schedule,
				Repeat:    p.Repeat,
				NextRunAt: nextRun,
			}
			if err := store.Save(job); err != nil {
				return "", err
			}
			return fmt.Sprintf("提醒已创建，ID=%d，将于 %s 触发", job.ID, nextRun.Format("2006-01-02 15:04")), nil
		},
	})

	registry.Register(&tools.Tool{
		Name:        "list_reminders",
		Description: "列出所有待触发的提醒",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
			jobs, err := store.List(agentID)
			if err != nil {
				return "", err
			}
			if len(jobs) == 0 {
				return "暂无提醒", nil
			}
			var sb strings.Builder
			for _, j := range jobs {
				repeat := "一次性"
				if j.Repeat {
					repeat = "重复"
				}
				fmt.Fprintf(&sb, "ID=%d | %s | %s | 下次: %s\n",
					j.ID, j.Message, repeat, j.NextRunAt.Format("2006-01-02 15:04"))
			}
			return sb.String(), nil
		},
	})

	registry.Register(&tools.Tool{
		Name:        "delete_reminder",
		Description: "删除一个提醒",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "integer", "description": "提醒 ID"},
			},
			"required": []string{"id"},
		},
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", err
			}
			if err := store.Delete(p.ID); err != nil {
				return "", err
			}
			return fmt.Sprintf("提醒 ID=%d 已删除", p.ID), nil
		},
	})
}
