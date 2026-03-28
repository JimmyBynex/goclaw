package tools

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

type Executor struct {
	registry *Registry
	timeout  time.Duration
}

func NewExecutor(registry *Registry, timeout time.Duration) *Executor {
	return &Executor{
		registry: registry,
		timeout:  timeout,
	}
}
func (e *Executor) Exec(ctx context.Context, call ToolUseBlock) ToolResultBlock {
	tool, ok := e.registry.Get(call.Name)
	//没权限或者没有
	if !ok {
		return ToolResultBlock{
			IsError:   true,
			ToolUseID: call.ID,
			Content:   fmt.Sprintf("%s is not found", call.Name),
		}
	}
	//为工具执行设置超时
	toolctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	log.Printf("[tools]Executing tool %s with input %s\n", call.Name, string(call.Input))
	output, err := tool.Execute(toolctx, call.Input)
	//执行工具错误
	if err != nil {
		log.Printf("[tools] %s failed: %v", call.Name, err)
		return ToolResultBlock{
			ToolUseID: call.ID,
			Content:   fmt.Sprintf("Error: %v", err),
			IsError:   true,
		}
	}
	log.Printf("[tools] %s succeeded tool %s", tool.Name, truncate(output, 200))
	return ToolResultBlock{
		ToolUseID: call.ID,
		Content:   output,
		IsError:   false,
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func (e *Executor) ExecuteAll(ctx context.Context, calls []ToolUseBlock) []ToolResultBlock {
	results := make([]ToolResultBlock, len(calls))
	if len(calls) == 1 {
		results[0] = e.Exec(ctx, calls[0])
		return results
	}
	var wg sync.WaitGroup
	for idx, call := range calls {
		wg.Add(1)
		go func(ctx context.Context, call ToolUseBlock) {
			defer wg.Done()
			//这里不能append，对于slice和map这种不是线程安全的
			results[idx] = e.Exec(ctx, call)
		}(ctx, call)
	}
	wg.Wait()
	return results
}
