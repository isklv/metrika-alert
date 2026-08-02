package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// Load reads config from path, then applies environment variable overrides.
// Supported env vars (name, colon-separated list of admin IDs, proxy URL, etc.):
//   METRIKA_DB_PATH        — database file path
//   METRIKA_DB_DIR         — directory for relative db paths
//   METRIKA_BOT_TOKEN      — telegram bot token
//   METRIKA_ADMIN_IDS      — comma-separated admin user IDs
//   METRIKA_METRIKA_BASE   — metrika API base URL (default: api-metrika.yandex.ru)
//   METRIKA_METRIKA_LOGS   — logs API base URL (default: logs.metrika.yandex.ru)
//   METRIKA_API_LISTEN     — REST API listen address (default :8090)
//   METRIKA_API_ENABLED    — "true" to enable the REST API
//   METRIKA_REPORT_HOURS   — periodic report interval in hours (default 1)
//   METRIKA_PROXY_URL      — proxy URL for telegram (socks5:// or http://)
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

	applyEnvOverrides(&c)

	return &c, nil
}

// applyEnvOverrides applies METRIKA_* environment variable overrides.
func applyEnvOverrides(c *Config) {
	if v := os.Getenv("METRIKA_DB_PATH"); v != "" {
		c.Database = v
	}
	if v := os.Getenv("METRIKA_DB_DIR"); v != "" {
		c.Database = filepath.Join(v, c.Database)
	}
	if v := os.Getenv("METRIKA_BOT_TOKEN"); v != "" {
		c.Telegram.BotToken = v
	}
	if v := os.Getenv("METRIKA_ADMIN_IDS"); v != "" {
		ids, err := parseAdminIDs(v)
		if err == nil && len(ids) > 0 {
			c.Telegram.AdminIDs = ids
		}
	}
	if v := os.Getenv("METRIKA_METRIKA_BASE"); v != "" {
		c.Metrika.BaseURL = v
	}
	if v := os.Getenv("METRIKA_METRIKA_LOGS"); v != "" {
		c.Metrika.LogsURL = v
	}
	if v := os.Getenv("METRIKA_API_LISTEN"); v != "" {
		c.API.ListenAddr = v
	}
	if v := os.Getenv("METRIKA_API_ENABLED"); v != "" {
		c.API.Enabled = v == "true" || v == "1" || v == "yes"
	}
	if v := os.Getenv("METRIKA_REPORT_HOURS"); v != "" {
		if h, err := parsePositiveInt(v); err == nil && h > 0 {
			c.ReportHour = h
		}
	}
	if v := os.Getenv("METRIKA_PROXY_URL"); v != "" {
		c.Telegram.ProxyURL = v
	}
}

func parseAdminIDs(s string) ([]int64, error) {
	var ids []int64
	for _, part := range splitNonEmpty(s, ",") {
		part = trimSpace(part)
		if part == "" {
			continue
		}
		id, err := parseSignedInt(part)
		if err != nil {
			return nil, fmt.Errorf("admin_ids: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func parsePositiveInt(s string) (int, error) {
	s = trimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty value")
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

func parseSignedInt(s string) (int64, error) {
	s = trimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty value")
	}
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, part := range split(s, sep) {
		if p := trimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func split(s, sep string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i:i+len(sep)] == sep {
			out = append(out, s[start:i])
			start = i + len(sep)
		}
	}
	out = append(out, s[start:])
	return out
}

func trimSpace(s string) string {
	return strings.TrimSpace(s)
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
