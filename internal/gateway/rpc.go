package gateway

import (
	"context"
	"encoding/json"
	"log"
)

type HandlerFunc func(ctx context.Context, params json.RawMessage) (any, error)

type Router struct {
	handlers map[string]HandlerFunc
}

func NewRouter() *Router {
	return &Router{handlers: make(map[string]HandlerFunc)}
}

func (r *Router) Register(method string, h HandlerFunc) {
	r.handlers[method] = h
}

func (r *Router) Dispatch(ctx context.Context, frame RequestFrame) ResponseFrame {
	handler := r.handlers[frame.Method]
	if handler == nil {
		return ErrResponse(frame.ID, "METHOD_NOT_FOUND", "unknown method:"+frame.Method)
	}
	res, err := handler(ctx, frame.Params)
	if err != nil {
		log.Printf("[protocol]Failed to handle request: %v", err)
		return ErrResponse(frame.ID, "INTERNAL_ERROR", err.Error())
	}
	return OKResponse(frame.ID, res)

}
