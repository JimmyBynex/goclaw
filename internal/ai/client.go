package ai

import "context"

type Client interface {
	StreamChat(ctx context.Context, message []Message) (<-chan string, <-chan error)
}
