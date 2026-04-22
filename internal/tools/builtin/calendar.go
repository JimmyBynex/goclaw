package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"goclaw/internal/structured"
	"goclaw/internal/tools"
)

func RegisterCalendarTools(registry *tools.Registry, store *structured.EventStore, agentID string) {
	registry.Register(&tools.Tool{
		Name:        "create_event",
		Description: "创建一个日程。start_at 和 end_at 使用 RFC3339 格式或常见格式（如 2026-04-23 14:00）。type 可选 one_time/task/recurring。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":    map[string]any{"type": "string", "description": "日程标题"},
				"type":     map[string]any{"type": "string", "description": "类型: one_time/task/recurring", "enum": []string{"one_time", "task", "recurring"}},
				"start_at": map[string]any{"type": "string", "description": "开始时间"},
				"end_at":   map[string]any{"type": "string", "description": "结束时间（可选）"},
				"location": map[string]any{"type": "string", "description": "地点（可选）"},
				"note":     map[string]any{"type": "string", "description": "备注（可选）"},
			},
			"required": []string{"title", "start_at"},
		},
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				Title    string `json:"title"`
				Type     string `json:"type"`
				StartAt  string `json:"start_at"`
				EndAt    string `json:"end_at"`
				Location string `json:"location"`
				Note     string `json:"note"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", err
			}
			startAt, err := parseTime(p.StartAt)
			if err != nil {
				return "", fmt.Errorf("无法解析开始时间: %w", err)
			}
			eventType := p.Type
			if eventType == "" {
				eventType = "one_time"
			}
			e := &structured.Event{
				AgentID:  agentID,
				Title:    p.Title,
				Type:     eventType,
				StartAt:  startAt,
				Location: p.Location,
				Note:     p.Note,
			}
			if p.EndAt != "" {
				endAt, err := parseTime(p.EndAt)
				if err != nil {
					return "", fmt.Errorf("无法解析结束时间: %w", err)
				}
				e.EndAt = &endAt
			}
			if err := store.Save(e); err != nil {
				return "", err
			}
			return fmt.Sprintf("日程已创建，ID=%d，%s 于 %s", e.ID, e.Title, startAt.Format("2006-01-02 15:04")), nil
		},
	})

	registry.Register(&tools.Tool{
		Name:        "list_events",
		Description: "列出指定时间范围内的日程。from 和 to 使用 RFC3339 或常见格式。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"from": map[string]any{"type": "string", "description": "开始时间"},
				"to":   map[string]any{"type": "string", "description": "结束时间"},
			},
			"required": []string{"from", "to"},
		},
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				From string `json:"from"`
				To   string `json:"to"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", err
			}
			from, err := parseTime(p.From)
			if err != nil {
				return "", fmt.Errorf("无法解析开始时间: %w", err)
			}
			to, err := parseTime(p.To)
			if err != nil {
				return "", fmt.Errorf("无法解析结束时间: %w", err)
			}
			events, err := store.ListByRange(agentID, from, to)
			if err != nil {
				return "", err
			}
			if len(events) == 0 {
				return "该时间范围内没有日程", nil
			}
			var sb strings.Builder
			for _, e := range events {
				fmt.Fprintf(&sb, "ID=%d | %s | %s", e.ID, e.StartAt.Format("2006-01-02 15:04"), e.Title)
				if e.Location != "" {
					fmt.Fprintf(&sb, " @ %s", e.Location)
				}
				sb.WriteByte('\n')
			}
			return sb.String(), nil
		},
	})

	registry.Register(&tools.Tool{
		Name:        "delete_event",
		Description: "删除一个日程",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "integer", "description": "日程 ID"},
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
			return fmt.Sprintf("日程 ID=%d 已删除", p.ID), nil
		},
	})
}

func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, f := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(f, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported time format: %s", s)
}
