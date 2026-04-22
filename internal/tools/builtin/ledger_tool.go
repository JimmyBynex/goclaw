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

func RegisterLedgerTools(registry *tools.Registry, store *structured.LedgerStore, agentID string) {
	registry.Register(&tools.Tool{
		Name:        "add_transaction",
		Description: "记录一笔收支。amount 正数为收入，负数为支出。happened_at 为实际发生时间（可选，默认当前时间）。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"amount":      map[string]any{"type": "number", "description": "金额，正数=收入，负数=支出"},
				"category":    map[string]any{"type": "string", "description": "分类，如：餐饮/交通/学习/工资/other"},
				"note":        map[string]any{"type": "string", "description": "备注（可选）"},
				"happened_at": map[string]any{"type": "string", "description": "实际发生时间（可选，默认当前时间）"},
			},
			"required": []string{"amount", "category"},
		},
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				Amount     float64 `json:"amount"`
				Category   string  `json:"category"`
				Note       string  `json:"note"`
				HappenedAt string  `json:"happened_at"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", err
			}
			happenedAt := time.Now()
			if p.HappenedAt != "" {
				t, err := parseTime(p.HappenedAt)
				if err != nil {
					return "", fmt.Errorf("无法解析时间: %w", err)
				}
				happenedAt = t
			}
			tx := &structured.Transaction{
				AgentID:    agentID,
				Amount:     p.Amount,
				Category:   p.Category,
				Note:       p.Note,
				HappenedAt: happenedAt,
			}
			if err := store.Save(tx); err != nil {
				return "", err
			}
			direction := "支出"
			if p.Amount > 0 {
				direction = "收入"
			}
			return fmt.Sprintf("已记录%s %.2f 元，分类：%s", direction, abs(p.Amount), p.Category), nil
		},
	})

	registry.Register(&tools.Tool{
		Name:        "monthly_summary",
		Description: "查询某月的收支汇总，month 格式为 2006-01（如 2026-04）",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"month": map[string]any{"type": "string", "description": "月份，格式 2006-01"},
			},
			"required": []string{"month"},
		},
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				Month string `json:"month"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", err
			}
			summary, err := store.MonthlySummary(agentID, p.Month)
			if err != nil {
				return "", err
			}
			if len(summary.ByCategory) == 0 {
				return fmt.Sprintf("%s 暂无记录", p.Month), nil
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "%s 收支汇总：\n", summary.Month)
			for cat, amount := range summary.ByCategory {
				fmt.Fprintf(&sb, "  %s：%.2f 元\n", cat, amount)
			}
			fmt.Fprintf(&sb, "合计：%.2f 元", summary.Total)
			return sb.String(), nil
		},
	})
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
