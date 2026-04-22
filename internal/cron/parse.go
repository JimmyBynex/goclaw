package cron

import (
	"fmt"
	"time"

	robfig "github.com/robfig/cron/v3"
)

// ParseNextRun 解析 schedule 字符串，返回下次触发时间
// repeat=false: RFC3339 或常见格式，from 参数忽略
// repeat=true: cron 表达式，从 from 开始计算下次时间
func ParseNextRun(schedule string, repeat bool, from time.Time) (time.Time, error) {
	if repeat {
		parser := robfig.NewParser(robfig.Minute | robfig.Hour | robfig.Dom | robfig.Month | robfig.Dow)
		sched, err := parser.Parse(schedule)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid cron expression %q: %w", schedule, err)
		}
		return sched.Next(from), nil
	}

	if t, err := time.Parse(time.RFC3339, schedule); err == nil {
		return t, nil
	}
	for _, f := range []string{"2006-01-02 15:04", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(f, schedule, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported time format: %s", schedule)
}
