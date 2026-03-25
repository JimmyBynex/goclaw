package ai

import (
	"errors"
	"log"
	"sync"
)

var factories = map[string]ModelFactory{}
var mu sync.RWMutex

type ModelFactory func(apiKey, model string) Client

func RegisterModelFactory(provider string, f ModelFactory) {
	mu.Lock()
	defer mu.Unlock()
	factories[provider] = f
}

func NewClient(provider, apiKey, model string) (Client, error) {
	mu.RLock()
	factory, ok := factories[provider]
	if !ok {
		mu.RUnlock()
		log.Println("[ai] no factory found for provider", provider)
		return nil, errors.New("invalid provider")
	}
	mu.RUnlock()
	return factory(apiKey, model), nil

}
