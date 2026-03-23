package config

type Config struct {
	Gateway  GatewayConfig  `yaml:"gateway"`
	Session  SessionConfig  `yaml:"session"`
	AI       AIConfig       `yaml:"ai"`
	Telegram TelegramConfig `yaml:"telegram"`
}

type GatewayConfig struct {
	Port   int    `yaml:"port"`
	Token  string `yaml:"token"`
	Bind   string `yaml:"bind"`
	Reload string `yaml:"reload"`
}

type SessionConfig struct {
	Dir          string `yaml:"dir"`
	MaxIdleHours int    `yaml:"max_idle_hours"`
	MaxMessages  int    `yaml:"max_messages"`
}

type AIConfig struct {
	Provider        string `yaml:"provider"`
	ApiKey          string `yaml:"api_key"`
	Model           string `yaml:"model"`
	SystemPrompt    string `yaml:"system_prompt"`
	MaxContextPairs int    `yaml:"max_context_pairs"`
	MaxTokens       int    `yaml:"max_tokens"`
}

type TelegramConfig struct {
	Token     string `yaml:"token"`
	AccountId string `yaml:"account_id"`
}
