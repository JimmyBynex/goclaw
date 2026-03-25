package agent

import (
	"context"
	"errors"
	"goclaw/internal/ai"
	"goclaw/internal/session"
	"log"
	"strings"
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
	eventCh chan<- AgentEvent,
) (*RunResult, error) {
	log.Printf("[agent] runAttempt: provider=%s model=%s", modelRef.Provider, modelRef.Model)
	client, err := ai.NewClient(modelRef.Provider, modelRef.APIkey,
		modelRef.Model)
	if err != nil {
		return nil, err
	}

	textCh, errCh := client.StreamChat(ctx, sess.MessagesForAI(a.systemPrompt,
		20))
	var fullReply strings.Builder
	for {
		log.Printf("[agent] select waiting...")
		log.Printf("[agent] textCh len: %d", len(textCh))
		select {
		case chunk, ok := <-textCh:
			if !ok {
				log.Printf("[agent] stream done, reply length: %d", len(fullReply.String()))
				return &RunResult{RunID: runID, Reply: fullReply.String(),
					Model: modelRef.Model}, nil
			}
			fullReply.WriteString(chunk)
			sendEvent(eventCh, AgentEvent{Type: "chat.delta", RunID: runID,
				Data: map[string]string{"chunk": chunk}})
		case err, ok := <-errCh:
			if !ok {
				errCh = nil // nil channel 永远不会被 select 选中
				continue
			}
			if err != nil {
				return nil, err
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
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
