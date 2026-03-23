package config

import (
	"context"
	"log"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
)

type OnChangeFunc func(old, new *Config)

type Manager struct {
	path     string                 //配置文件路径
	ptr      atomic.Pointer[Config] //存当前配置的原子指针
	watcher  *fsnotify.Watcher      //监听文件系统事件
	handlers []OnChangeFunc         //配置变更后要通知谁
}

func NewManager(path string) (*Manager, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	err = watcher.Add(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	var m Manager
	m.path = path
	m.watcher = watcher
	m.ptr.Store(cfg)
	return &m, nil

}

func (m *Manager) Get() *Config {
	cfg := m.ptr.Load()
	return cfg
}

func (m *Manager) OnChange(fn OnChangeFunc) {
	m.handlers = append(m.handlers, fn)
}

func (m *Manager) Watch(ctx context.Context) error {
	for {
		select {
		case event := <-m.watcher.Events:
			//write是普通编辑器，create是vim编辑器，因为是先删后改
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				m.reload()
			}
			if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				m.watcher.Add(m.path) //重新登记具体文件监听注册表
			}
		case err := <-m.watcher.Errors:
			log.Println("[]watcher error:", err)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *Manager) reload() {
	cfg, err := Load(m.path)
	if err != nil {
		log.Println("[config]reload error:", err)
		return
	}
	old := m.ptr.Swap(cfg)
	for _, fn := range m.handlers {
		go fn(old, cfg) // 异步通知，不阻塞监听循环
	}
}
