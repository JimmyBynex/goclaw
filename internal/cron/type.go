// internal/cron/types.go

package cron

import "time"

// Job 是一条定时任务
type Job struct {
	ID        int64
	AgentID   string    // 属于哪个 Agent
	ChannelID string    // 发往哪个渠道（"telegram"）
	AccountID string    // 渠道账号 ID
	PeerID    string    // 发给谁（用户/群组 ID）
	Message   string    // 到点发送的消息内容
	Schedule  string    // cron 表达式（"0 8 * * *" = 每天8点）或 RFC3339 一次性时间
	Repeat    bool      // false=执行一次后删除，true=按 schedule 重复
	NextRunAt time.Time // 下次执行时间（调度器填写）
	CreatedAt time.Time
	Done      bool // 一次性任务完成后标记
}
