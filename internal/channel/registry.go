package channel

import (
	"fmt"
	"sync"
)

// Registry 就是一张"名字 → 创建方式"的表,并非在同一个主体运行，外部注册
var registry = map[string]Factory{}
var mu sync.RWMutex

func Register(id string, f Factory) {
	mu.Lock()
	registry[id] = f
	mu.Unlock()
}

// create才是创建实例的函数，start是主流程，register是添加到注册表
// ch, err := channel.Create("telegram", "bot001", cfg, handler)使用示例
func Create(id, accountID string, cfg map[string]any, handler InBoundHandler) (Channel, error) {
	mu.RLock()
	defer mu.RUnlock()
	f, ok := registry[id]
	if !ok {
		return nil, fmt.Errorf("no factory found for id %s", id)
	}
	return f(accountID, cfg, handler)
}
