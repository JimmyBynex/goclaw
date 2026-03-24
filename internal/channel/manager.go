package channel

import (
	"context"
	"fmt"
	"log"
	"sync"
)

type entry struct {
	ch     Channel
	cancel context.CancelFunc
	done   chan struct{} //相当于一个位的标志位
}
type Manager struct {
	handler InBoundHandler    // 注入给每个 Channel 的回调,相当于gateway告诉channel，收到的东西都交给我
	mu      sync.RWMutex      // 保护 entries
	entries map[string]*entry // 正在运行的渠道
}

// 从gateway传进来
func NewManager(handler InBoundHandler) *Manager {
	return &Manager{
		handler: handler,
		entries: make(map[string]*entry),
		mu:      sync.RWMutex{},
	}
}

func (m *Manager) Start(ctx context.Context, channelID, accountID string, cfg map[string]any) error {
	//一个平台可以有多个账户 channelID + ":" + accountID
	key := channelID + ":" + accountID

	//先检查是否已经在运行
	m.mu.Lock()
	if _, exists := m.entries[key]; exists {
		m.mu.Unlock()
		return fmt.Errorf("channel %s already running", key)
	}
	m.mu.Unlock()
	//再新建
	ch, err := Create(channelID, accountID, cfg, m.handler)
	if err != nil {
		log.Printf("[channel]failed to create: %v", err)
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	//加上done方式
	go func() {
		ch.Start(ctx)
		close(done)
	}()

	m.mu.Lock()
	m.entries[key] = &entry{
		ch:     ch,
		cancel: cancel,
		done:   done,
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) Stop(channelID, accountID string) error {
	key := channelID + ":" + accountID

	m.mu.Lock()
	e, exists := m.entries[key]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("channel %s not running", key)
	}
	delete(m.entries, key)
	m.mu.Unlock()

	e.cancel()
	<-e.done
	return nil
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	entries := m.entries //先拿出来，而不是边遍历边删除
	m.entries = make(map[string]*entry)
	m.mu.Unlock()

	var wg sync.WaitGroup //并行关闭
	for _, e := range entries {
		wg.Add(1)
		go func(e *entry) {
			defer wg.Done()
			e.cancel()
			<-e.done
		}(e)
	}
	wg.Wait()
}

func (m *Manager) Get(channelID, accountID string) (Channel,
	error) {
	key := channelID + ":" + accountID
	m.mu.RLock()
	e, ok := m.entries[key]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("channel %s not running", key)
	}
	return e.ch, nil
}
