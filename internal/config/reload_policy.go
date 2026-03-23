package config

type ReloadDecision int

const (
	ReloadNone    ReloadDecision = iota // 热更新，直接生效
	ReloadChannel                       // 重启 Telegram 连接
	ReloadGateway                       // 重启 Gateway
)

func Diff(old, new *Config) ReloadDecision {
	if old.Gateway.Port != new.Gateway.Port {
		return ReloadGateway
	}
	if old.Telegram.Token != new.Telegram.Token {
		return ReloadChannel
	}
	return ReloadNone
}
