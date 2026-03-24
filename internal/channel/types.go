package channel

import (
	"context"
	"time"
)

// 客户端收到消息转换成inbondMessage，在调用inboundHanlder
type InBoundMessage struct {
	ChannelID string // "telegram" / "discord"
	AccountID string // 哪个账号
	PeerID    string // 回复目标（私聊是用户ID，群组是群ID）
	ChatType  string // "private" / "group"
	UserID    string // 具体发送者
	Text      string
	Timestamp time.Time
	Raw       any //方便之后提取特定字段
}

// 发送到渠道的标准化消息,少的这些字段无所谓
type OutboundMessage struct {
	Text      string
	PeerID    string
	ParseMode string //"markdown,html"
}

type InBoundHandler func(ctx context.Context, msg InBoundMessage)

type Channel interface {
	Send(ctx context.Context, msg OutboundMessage) (id string, err error)
	Start(ctx context.Context) error
	Stop() error
	Status() ChannelStatus
	ID() string
	AccountID() string
}

type StreamSender interface {
	SendStream(ctx context.Context, peerID string, textCh <-chan string) error
}

type ChannelStatus struct {
	Connected bool
	AccountID string
	Error     string
	Since     time.Time
}

type Factory func(accountID string, cfg map[string]any, handler InBoundHandler) (Channel, error)
