// internal/tools/builtin/calculator.go

package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"go/constant"
	"go/token"
	"goclaw/internal/tools"
)

// CalculateTool 执行简单数学计算
// 使用 Go 标准库解析数学表达式，安全且无需第三方依赖
var CalculateTool = &tools.Tool{
	Name:        "calculate",
	Description: "Evaluate a mathematical expression. Supports +, -, *, /, ** (power), and parentheses.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"expression": map[string]any{
				"type":        "string",
				"description": "Mathematical expression to evaluate, e.g. '(3 + 4) * 2'",
			},
		},
		"required": []string{"expression"},
	},
	Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
		var params struct {
			Expression string `json:"expression"`
		}
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.Expression == "" {
			return "", fmt.Errorf("expression is required")
		}

		// 使用 go/constant 安全求值（只支持常量表达式，防止注入）
		val := constant.MakeFromLiteral(params.Expression, token.FLOAT, 0)
		if val.Kind() == constant.Unknown {
			return "", fmt.Errorf("cannot evaluate expression: %q", params.Expression)
		}
		result, _ := constant.Float64Val(val)
		return fmt.Sprintf("%g", result), nil
	},
}
