package gateway

import (
	"context"
	"encoding/json"
	"runtime"
	"time"
)

type HealthHandler struct {
	startTime time.Time
}

func NewHealthHandler() *HealthHandler {
	return &HealthHandler{startTime: time.Now()}
}

type HealthResult struct {
	OK         bool   `json:"ok"`
	Uptime     string `json:"uptime"`
	Goroutines int    `json:"goroutines"`
	MemMB      uint64 `json:"mem_mb"`
}

func (h *HealthHandler) Health(ctx context.Context, _ json.RawMessage) (any, error) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	return HealthResult{
		OK:         true,
		Uptime:     time.Since(h.startTime).String(),
		Goroutines: runtime.NumGoroutine(),
		MemMB:      mem.Alloc / 1024 / 1024,
	}, nil
}
