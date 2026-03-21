package gateway

import (
	"encoding/json"
	"log"
)

type RequestFrame struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type ResponseFrame struct {
	ID    string          `json:"id"`
	Data  json.RawMessage `json:"data,omitempty"`  //这个是引用类型，还包括slice，map，[]byte，指针，能触发omitempty
	Error *RPCError       `json:"error,omitempty"` //所以这个必须修改为指针类型
}

type EventFrame struct {
	//Gateway主动推的事件
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type RPCError struct {
	// 错误结构
	Code    string `json:"code"`
	Message string `json:"message"`
}

func OKResponse(id string, data any) ResponseFrame {
	response, err := json.Marshal(data)
	if err != nil {
		log.Printf("[protocol]Failed to marshal response: %v", err)
		return ResponseFrame{ID: id}
	}
	return ResponseFrame{ID: id, Data: response}
}

func ErrResponse(id, code, message string) ResponseFrame {
	return ResponseFrame{
		ID: id,
		Error: &RPCError{
			Code:    code,
			Message: message,
		},
	}
}
func NewEvent(eventType string, payload any) EventFrame {
	pl, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[protocol]Failed to marshal payload: %v", err)
		return EventFrame{Type: eventType}
	}
	return EventFrame{Type: eventType, Payload: pl}

}
