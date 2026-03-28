package agent

import (
	"context"
	"errors"
	"fmt"
	"goclaw/internal/ai"
	"goclaw/internal/session"
	"log"
)

// 最终结果
type RunResult struct {
	RunID string
	Reply string
	Model string
}

// 推理过程
type AgentEvent struct {
	Type  string
	RunID string
	Data  any
}

// 主动错误
var ErrAborted = errors.New("inference aborted")

// 每次重新构建aiclient
func (a *Agent) runAttempt(
	ctx context.Context,
	modelRef ModelRef,
	sess *session.Session,
	runID string,
	eventCh chan<- AgentEvent, //为什么还需要enventCh，因为还需要通知gateway
) (*RunResult, error) {
	client, err := ai.NewClient(modelRef.Provider, modelRef.APIKey, modelRef.Model)
	if err != nil {
		return nil, err
	}

	// 为当前 Agent 过滤可用工具
	agentTools := a.toolRegistry.FilterForAgent(a.id)
	toolDefs := agentTools.Definitions()

	// 工具调用循环
	// 消息历史在循环内增长（加入工具结果），不写入持久化 Session
	// 只有最终的文本回复才写入 Session
	loopMessages := sess.MessagesForAI(a.systemPrompt, 20)
	maxIterations := 10 // 防止无限循环

	for i := 0; i < maxIterations; i++ {
		// 发起 AI 请求
		resp, err := client.Chat(ctx, loopMessages, toolDefs)
		if err != nil {
			return nil, err
		}

		switch resp.StopReason {
		case "end_turn":
			// AI 完成，返回文字结果
			return &RunResult{
				RunID: runID,
				Reply: resp.Text,
				Model: modelRef.Model,
			}, nil

		case "tool_use":
			// AI 要调用工具
			if len(resp.ToolCalls) == 0 {
				return nil, fmt.Errorf("tool_use stop reason but no tool calls")
			}

			// 通知 Gateway AI 正在调用工具
			sendEvent(eventCh, AgentEvent{
				Type:  "agent.tool_calls",
				RunID: runID,
				Data:  resp.ToolCalls,
			})

			// 将 AI 的工具调用请求加入消息历史
			loopMessages = append(loopMessages, ai.Message{
				Role:      "assistant",
				ToolCalls: resp.ToolCalls,
			})

			// 并发执行所有工具
			results := a.executor.ExecuteAll(ctx, resp.ToolCalls)

			// 通知 Gateway 工具执行结果
			sendEvent(eventCh, AgentEvent{
				Type:  "agent.tool_results",
				RunID: runID,
				Data:  results,
			})

			// 将工具结果加入消息历史，供 AI 继续推理
			loopMessages = append(loopMessages, ai.Message{
				Role:        "tool",
				ToolResults: results,
			})

			// 继续下一轮循环（AI 会看到工具结果，决定下一步）

		default:
			return nil, fmt.Errorf("unexpected stop reason: %s", resp.StopReason)
		}
	}

	return nil, fmt.Errorf("tool loop exceeded max iterations (%d)", maxIterations)
}

// 防止下游消费受限，影响推理
func sendEvent(ch chan<- AgentEvent, e AgentEvent) {
	if ch == nil {
		return
	}
	select {
	case ch <- e:
	default:
	}
}

// 阻塞使用每个可使用的模型，直到主动停止或者全部失败
func (a *Agent) runWithFallback(
	ctx context.Context,
	sess *session.Session,
	runID string,
	eventCh chan<- AgentEvent,
) (*RunResult, error) {
	var lastErr error
	for _, model := range a.models {
		select {
		case <-ctx.Done():
			return nil, ErrAborted
		default:
		}
		result, err := a.runAttempt(ctx, model, sess, runID, eventCh)
		if err != nil {
			log.Printf("[agent]runWithFallback: model=%s runID=%s err=%v",
				model.Model, runID, err)
			lastErr = err
			continue
		}
		return result, nil
	}
	return nil, lastErr
}

func (a *Agent) RunReply(
	parentCtx context.Context,
	sess *session.Session,
	userText string,
	runID string,
	eventCh chan<- AgentEvent,
) (*RunResult, error) {
	ctx, cancel := a.abortReg.Register(parentCtx, runID)
	defer func() {
		cancel()
		a.abortReg.Unregister(runID)
	}()
	sess.AddUserMessage(userText)
	result, err := a.runWithFallback(ctx, sess, runID, eventCh)
	if err != nil {
		return nil, err
	}
	sess.AddAssistantMessage(result.Reply)
	a.store.Save(sess)
	return result, nil
}
