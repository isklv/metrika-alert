package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds the service configuration.
type Config struct {
	Database   string      `yaml:"database"`
	Telegram   TelegramCfg `yaml:"telegram"`
	VKTeams    VKTeamsCfg  `yaml:"vkteams"`
	Metrika    MetrikaCfg  `yaml:"metrika"`
	API        APICfg      `yaml:"api"`
	ReportHour int         `yaml:"report_interval_hours"` // hours between periodic reports, 0 disables
}

type TelegramCfg struct {
	BotToken string  `yaml:"bot_token"`
	AdminIDs []int64 `yaml:"admin_ids"` // user IDs allowed to configure the bot
	ProxyURL string  `yaml:"proxy_url"` // socks5:// or http:// proxy for Telegram API
}

// VKTeamsCfg configures the VK Teams (Mail.ru Myteam) bot.
type VKTeamsCfg struct {
	BotToken    string   `yaml:"bot_token"`
	BaseURL     string   `yaml:"base_url"`  // bot API endpoint, cloud by default
	AdminIDs    []string `yaml:"admin_ids"` // VK Teams user IDs (email- or numeric-like strings)
	ProxyURL    string   `yaml:"proxy_url"`
	IgnoreTLS   bool     `yaml:"ignore_tls"`
	InsecureTLS bool     `yaml:"insecure_tls,omitempty"`
}

type MetrikaCfg struct {
	// BaseURL serves both the Reporting API (/stat/v1/data) and the Logs API
	// (/management/v1/...), so one host covers everything.
	BaseURL string `yaml:"base_url"`
	// SettleMinutes is how long data is left to settle before it is judged.
	// Metrika keeps counting sessions for a short while after they happen, so
	// measuring right up to the present reads low.
	SettleMinutes int `yaml:"settle_minutes"`
	// WindowMinutes is how much traffic one measurement covers.
	WindowMinutes int `yaml:"window_minutes"`
	// StepMinutes is how often the window advances, and so how quickly a drop
	// is noticed.
	StepMinutes int `yaml:"step_minutes"`
	// MaxCatchUpSteps bounds how many missed windows one check works through,
	// so a counter idle for a week does not fire a burst of stale alerts.
	MaxCatchUpSteps int `yaml:"max_catch_up_steps"`
}

type APICfg struct {
	ListenAddr string `yaml:"listen_addr"`
	Enabled    bool   `yaml:"enabled"`
	AuthToken  string `yaml:"auth_token"` // when set, requests must carry it as a bearer token
}

// DefaultMetrikaURL is the documented Yandex.Metrika API host. Note the .net
// domain — api-metrika.yandex.ru does not serve the API.
const DefaultMetrikaURL = "https://api-metrika.yandex.net"

// legacyMetrikaHosts are values earlier releases shipped as defaults. Neither
// answers the documented API paths, so a config still carrying one is corrected
// rather than left to fail every call with a 404.
var legacyMetrikaHosts = map[string]bool{
	"https://api-metrika.yandex.ru":  true,
	"https://logs.metrika.yandex.ru": true,
}

// Measurement defaults: an hour-wide window advancing every ten minutes.
const (
	DefaultSettleMinutes   = 20
	DefaultWindowMinutes   = 60
	DefaultStepMinutes     = 10
	DefaultMaxCatchUpSteps = 6
)

// DefaultVKTeamsURL is the VK Teams cloud bot API. On-premise installations
// override it via vkteams.base_url or METRIKA_VKTEAMS_BASE.
const DefaultVKTeamsURL = "https://myteam.mail.ru/bot/v1"

// Load reads config from path, then applies environment variable overrides.
// Supported env vars:
//
//	METRIKA_DB_PATH         — database file path
//	METRIKA_DB_DIR          — directory for relative db paths
//	METRIKA_BOT_TOKEN       — telegram bot token
//	METRIKA_ADMIN_IDS       — comma-separated telegram admin user IDs
//	METRIKA_PROXY_URL       — proxy URL for telegram (socks5:// or http://)
//	METRIKA_VKTEAMS_TOKEN   — VK Teams bot token
//	METRIKA_VKTEAMS_BASE    — VK Teams bot API base URL
//	METRIKA_VKTEAMS_ADMINS  — comma-separated VK Teams admin user IDs
//	METRIKA_VKTEAMS_PROXY   — proxy URL for VK Teams
//	METRIKA_VKTEAMS_IGNORE_TLS — "true" to ignore TLS certificate errors
//	METRIKA_METRIKA_BASE    — metrika API base URL
//	METRIKA_SETTLE_MINUTES  — delay before a closed hour is judged
//	METRIKA_WINDOW_MINUTES  — width of one measurement window
//	METRIKA_STEP_MINUTES    — how often the window advances
//	METRIKA_MAX_CATCH_UP_STEPS — how many missed windows one check works through
//	METRIKA_API_LISTEN      — REST API listen address
//	METRIKA_API_ENABLED     — "true" to enable the REST API
//	METRIKA_API_TOKEN       — bearer token required by the REST API
//	METRIKA_REPORT_HOURS    — periodic report interval in hours, 0 disables
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyEnvOverrides(&c)
	applyDefaults(&c)

	return &c, nil
}

// applyDefaults fills unset values. Runs after the environment so that an
// env var and a config key are treated identically.
func applyDefaults(c *Config) {
	if c.Metrika.BaseURL == "" || legacyMetrikaHosts[strings.TrimSuffix(c.Metrika.BaseURL, "/")] {
		if c.Metrika.BaseURL != "" {
			log.Printf("config: metrika.base_url %q does not serve the Metrika API, using %s",
				c.Metrika.BaseURL, DefaultMetrikaURL)
		}
		c.Metrika.BaseURL = DefaultMetrikaURL
	}
	if c.Metrika.SettleMinutes <= 0 {
		c.Metrika.SettleMinutes = DefaultSettleMinutes
	}
	if c.Metrika.WindowMinutes <= 0 {
		c.Metrika.WindowMinutes = DefaultWindowMinutes
	}
	if c.Metrika.StepMinutes <= 0 {
		c.Metrika.StepMinutes = DefaultStepMinutes
	}
	if c.Metrika.MaxCatchUpSteps <= 0 {
		c.Metrika.MaxCatchUpSteps = DefaultMaxCatchUpSteps
	}
	if c.VKTeams.BaseURL == "" {
		c.VKTeams.BaseURL = DefaultVKTeamsURL
	}
	if c.VKTeams.InsecureTLS {
		c.VKTeams.IgnoreTLS = true
	}
	if c.Database == "" {
		c.Database = "metrika.db"
	}
	if c.API.ListenAddr == "" {
		c.API.ListenAddr = ":8090"
	}
	if c.ReportHour < 0 {
		c.ReportHour = 0
	}
}

// applyEnvOverrides applies METRIKA_* environment variable overrides.
func applyEnvOverrides(c *Config) {
	if v := os.Getenv("METRIKA_DB_PATH"); v != "" {
		c.Database = v
	}
	// METRIKA_DB_DIR only relocates a relative path; an absolute METRIKA_DB_PATH wins.
	if v := os.Getenv("METRIKA_DB_DIR"); v != "" && !filepath.IsAbs(c.Database) {
		c.Database = filepath.Join(v, c.Database)
	}
	if v := os.Getenv("METRIKA_BOT_TOKEN"); v != "" {
		c.Telegram.BotToken = v
	}
	if v := os.Getenv("METRIKA_ADMIN_IDS"); v != "" {
		if ids, err := parseAdminIDs(v); err == nil && len(ids) > 0 {
			c.Telegram.AdminIDs = ids
		}
	}
	if v := os.Getenv("METRIKA_PROXY_URL"); v != "" {
		c.Telegram.ProxyURL = v
	}
	if v := os.Getenv("METRIKA_VKTEAMS_TOKEN"); v != "" {
		c.VKTeams.BotToken = v
	}
	if v := os.Getenv("METRIKA_VKTEAMS_BASE"); v != "" {
		c.VKTeams.BaseURL = v
	}
	if v := os.Getenv("METRIKA_VKTEAMS_ADMINS"); v != "" {
		if ids := splitNonEmpty(v, ","); len(ids) > 0 {
			c.VKTeams.AdminIDs = ids
		}
	}
	if v := os.Getenv("METRIKA_VKTEAMS_PROXY"); v != "" {
		c.VKTeams.ProxyURL = v
	}
	if v := os.Getenv("METRIKA_VKTEAMS_IGNORE_TLS"); v != "" {
		c.VKTeams.IgnoreTLS = v == "true" || v == "1" || v == "yes"
	} else if v := os.Getenv("METRIKA_VKTEAMS_INSECURE_TLS"); v != "" {
		c.VKTeams.IgnoreTLS = v == "true" || v == "1" || v == "yes"
	}
	if v := os.Getenv("METRIKA_METRIKA_BASE"); v != "" {
		c.Metrika.BaseURL = v
	}
	if v := os.Getenv("METRIKA_SETTLE_MINUTES"); v != "" {
		if m, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && m > 0 {
			c.Metrika.SettleMinutes = m
		}
	}
	if v := os.Getenv("METRIKA_WINDOW_MINUTES"); v != "" {
		if m, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && m > 0 {
			c.Metrika.WindowMinutes = m
		}
	}
	if v := os.Getenv("METRIKA_STEP_MINUTES"); v != "" {
		if m, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && m > 0 {
			c.Metrika.StepMinutes = m
		}
	}
	if v := os.Getenv("METRIKA_MAX_CATCH_UP_STEPS"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			c.Metrika.MaxCatchUpSteps = n
		}
	}
	if v := os.Getenv("METRIKA_API_LISTEN"); v != "" {
		c.API.ListenAddr = v
	}
	if v := os.Getenv("METRIKA_API_ENABLED"); v != "" {
		c.API.Enabled = v == "true" || v == "1" || v == "yes"
	}
	if v := os.Getenv("METRIKA_API_TOKEN"); v != "" {
		c.API.AuthToken = v
	}
	if v := os.Getenv("METRIKA_REPORT_HOURS"); v != "" {
		if h, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && h >= 0 {
			c.ReportHour = h
		}
	}
}

func parseAdminIDs(s string) ([]int64, error) {
	var ids []int64
	for _, part := range splitNonEmpty(s, ",") {
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("admin_ids: %q is not a user ID: %w", part, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, part := range strings.Split(s, sep) {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ResolvePath expands ~ and relative paths in a config value.
func ResolvePath(path string) string {
	if path == "" {
		return path
	}
	if len(path) > 1 && path[0] == '~' {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[1:])
		}
	}
	return filepath.Clean(path)
}
