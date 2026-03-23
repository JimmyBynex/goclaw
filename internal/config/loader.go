package config

import (
	"errors"
	"os"

	"gopkg.in/yaml.v3"
)

func Load(path string) (*Config, error) {
	text, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	config := WithDefaults()
	err = yaml.Unmarshal(text, &config)
	if err != nil {
		return nil, err
	}
	err = validate(&config)
	if err != nil {
		return nil, err
	}
	return &config, nil
}

func WithDefaults() Config {
	return Config{
		AI: AIConfig{
			MaxContextPairs: 20,
		},
		Gateway: GatewayConfig{
			Port: 18789,
		},
	}
}

func validate(cfg *Config) error {
	if cfg.Telegram.Token == "" {
		return errors.New("telegram.token is required")
	}
	if cfg.AI.ApiKey == "" {
		return errors.New("ai.api_key is required")
	}
	return nil
}
