package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config holds the service configuration.
type Config struct {
	Database   string     `yaml:"database"`
	Telegram   TelegramCfg `yaml:"telegram"`
	Metrika    MetrikaCfg  `yaml:"metrika"`
	API        APICfg      `yaml:"api"`
	ReportHour int         `yaml:"report_interval_hours"` // hours between periodic reports, default 1
}

type TelegramCfg struct {
	BotToken string   `yaml:"bot_token"`
	AdminIDs []int64  `yaml:"admin_ids"` // user IDs allowed to configure the bot
	ProxyURL string   `yaml:"proxy_url"` // socks5:// or http:// proxy for Telegram API
}

type MetrikaCfg struct {
	BaseURL string `yaml:"base_url"`
	LogsURL string `yaml:"logs_url"`
}

type APICfg struct {
	ListenAddr string `yaml:"listen_addr"`
	Enabled    bool   `yaml:"enabled"`
}

// DefaultMetrikaURLs returns Yandex.Metrika API endpoints.
func DefaultMetrikaURLs() (base, logs string) {
	return "https://api-metrika.yandex.ru", "https://logs.metrika.yandex.ru"
}

// Load reads config from path. Missing fields are filled with defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	baseURL, logsURL := DefaultMetrikaURLs()
	if c.Metrika.BaseURL == "" {
		c.Metrika.BaseURL = baseURL
	}
	if c.Metrika.LogsURL == "" {
		c.Metrika.LogsURL = logsURL
	}
	if c.Database == "" {
		c.Database = "metrika.db"
	}
	if c.API.ListenAddr == "" {
		c.API.ListenAddr = ":8090"
	}
	if c.ReportHour <= 0 {
		c.ReportHour = 1
	}

	return &c, nil
}

// ResolvePath expands ~ and relative paths in a config value.
func ResolvePath(path string) string {
	if path == "" {
		return path
	}
	if len(path) > 1 && path[0] == '~' {
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, path[1:])
		}
	}
	return filepath.Clean(path)
}
