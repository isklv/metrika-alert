package config

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleYAML = `
database: metrika.db
telegram:
  bot_token: "12345:ABC"
  admin_ids: [111, 222]
  proxy_url: socks5://proxy:1080
metrika:
  base_url: https://api.example.com
  logs_url: https://logs.example.com
api:
  enabled: true
  listen_addr: ":9090"
report_interval_hours: 6
`

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return p
}

// setEnv sets env vars for the duration of the test and restores them after.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoad_ValidConfig(t *testing.T) {
	p := writeTempConfig(t, sampleYAML)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database != "metrika.db" {
		t.Errorf("Database = %q, want %q", cfg.Database, "metrika.db")
	}
	if cfg.Telegram.BotToken != "12345:ABC" {
		t.Errorf("BotToken = %q, want %q", cfg.Telegram.BotToken, "12345:ABC")
	}
	if len(cfg.Telegram.AdminIDs) != 2 || cfg.Telegram.AdminIDs[0] != 111 || cfg.Telegram.AdminIDs[1] != 222 {
		t.Errorf("AdminIDs = %v, want [111 222]", cfg.Telegram.AdminIDs)
	}
	if cfg.Telegram.ProxyURL != "socks5://proxy:1080" {
		t.Errorf("ProxyURL = %q", cfg.Telegram.ProxyURL)
	}
	if cfg.Metrika.BaseURL != "https://api.example.com" {
		t.Errorf("BaseURL = %q", cfg.Metrika.BaseURL)
	}
	if cfg.Metrika.LogsURL != "https://logs.example.com" {
		t.Errorf("LogsURL = %q", cfg.Metrika.LogsURL)
	}
	if !cfg.API.Enabled {
		t.Errorf("API.Enabled = false, want true")
	}
	if cfg.API.ListenAddr != ":9090" {
		t.Errorf("ListenAddr = %q", cfg.API.ListenAddr)
	}
	if cfg.ReportHour != 6 {
		t.Errorf("ReportHour = %d, want 6", cfg.ReportHour)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	p := writeTempConfig(t, "database: [unclosed")
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for invalid yaml, got nil")
	}
}

func TestLoad_Defaults(t *testing.T) {
	p := writeTempConfig(t, "telegram:\n  bot_token: \"12345:ABC\"\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database != "metrika.db" {
		t.Errorf("default Database = %q, want metrika.db", cfg.Database)
	}
	if cfg.Metrika.BaseURL != "https://api-metrika.yandex.ru" {
		t.Errorf("default BaseURL = %q", cfg.Metrika.BaseURL)
	}
	if cfg.Metrika.LogsURL != "https://logs.metrika.yandex.ru" {
		t.Errorf("default LogsURL = %q", cfg.Metrika.LogsURL)
	}
	if cfg.API.ListenAddr != ":8090" {
		t.Errorf("default ListenAddr = %q", cfg.API.ListenAddr)
	}
	if cfg.ReportHour != 1 {
		t.Errorf("default ReportHour = %d, want 1", cfg.ReportHour)
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	p := writeTempConfig(t, sampleYAML)
	setEnv(t, map[string]string{
		"METRIKA_DB_PATH":      "/data/metrika.db",
		"METRIKA_BOT_TOKEN":    "99999:ZZZ",
		"METRIKA_ADMIN_IDS":    "7,8,9",
		"METRIKA_METRIKA_BASE": "https://base.env",
		"METRIKA_METRIKA_LOGS": "https://logs.env",
		"METRIKA_API_LISTEN":   ":1234",
		"METRIKA_API_ENABLED":  "false",
		"METRIKA_REPORT_HOURS": "12",
		"METRIKA_PROXY_URL":    "http://env-proxy:3128",
	})
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database != "/data/metrika.db" {
		t.Errorf("Database = %q, want env override", cfg.Database)
	}
	if cfg.Telegram.BotToken != "99999:ZZZ" {
		t.Errorf("BotToken = %q, want env override", cfg.Telegram.BotToken)
	}
	if len(cfg.Telegram.AdminIDs) != 3 || cfg.Telegram.AdminIDs[2] != 9 {
		t.Errorf("AdminIDs = %v, want [7 8 9]", cfg.Telegram.AdminIDs)
	}
	if cfg.Metrika.BaseURL != "https://base.env" {
		t.Errorf("BaseURL = %q", cfg.Metrika.BaseURL)
	}
	if cfg.Metrika.LogsURL != "https://logs.env" {
		t.Errorf("LogsURL = %q", cfg.Metrika.LogsURL)
	}
	if cfg.API.ListenAddr != ":1234" {
		t.Errorf("ListenAddr = %q", cfg.API.ListenAddr)
	}
	if cfg.API.Enabled {
		t.Errorf("API.Enabled = true, want false (env override)")
	}
	if cfg.ReportHour != 12 {
		t.Errorf("ReportHour = %d, want 12", cfg.ReportHour)
	}
	if cfg.Telegram.ProxyURL != "http://env-proxy:3128" {
		t.Errorf("ProxyURL = %q", cfg.Telegram.ProxyURL)
	}
}

func TestLoad_EnvDBDirJoinsRelativePath(t *testing.T) {
	p := writeTempConfig(t, "database: metrika.db\n")
	setEnv(t, map[string]string{"METRIKA_DB_DIR": "/var/lib/metrika-alert"})
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := "/var/lib/metrika-alert/metrika.db"; cfg.Database != want {
		t.Errorf("Database = %q, want %q", cfg.Database, want)
	}
}

func TestLoad_EnvOnlyNoFile(t *testing.T) {
	// Env vars must work even when the config file has no such keys.
	p := writeTempConfig(t, "telegram:\n  bot_token: \"1:2\"\n")
	setEnv(t, map[string]string{
		"METRIKA_API_ENABLED": "1",
	})
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.API.Enabled {
		t.Errorf("API.Enabled = false, want true from env '1'")
	}
}

func TestLoad_EnvInvalidValuesIgnored(t *testing.T) {
	p := writeTempConfig(t, sampleYAML)
	setEnv(t, map[string]string{
		"METRIKA_ADMIN_IDS":    "7,notanumber,9",
		"METRIKA_API_ENABLED":  "banana",
		"METRIKA_REPORT_HOURS": "x",
	})
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// parseAdminIDs errors on any non-numeric part, so file value is preserved.
	if len(cfg.Telegram.AdminIDs) != 2 || cfg.Telegram.AdminIDs[0] != 111 || cfg.Telegram.AdminIDs[1] != 222 {
		t.Errorf("AdminIDs = %v, want file value [111 222] preserved", cfg.Telegram.AdminIDs)
	}
	// Invalid bool ("banana") disables the API.
	if cfg.API.Enabled {
		t.Errorf("API.Enabled = true, want false from bad env bool")
	}
	// Invalid int leaves file value (6 in sampleYAML).
	if cfg.ReportHour != 6 {
		t.Errorf("ReportHour = %d, want 6", cfg.ReportHour)
	}
}

func TestLoad_EnvReportHoursZeroIgnored(t *testing.T) {
	// parsePositiveInt returns 0 for "0" and the override only applies when h > 0.
	p := writeTempConfig(t, "report_interval_hours: 5\n")
	setEnv(t, map[string]string{"METRIKA_REPORT_HOURS": "0"})
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ReportHour != 5 {
		t.Errorf("ReportHour = %d, want file value 5 preserved", cfg.ReportHour)
	}
}

func TestResolvePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got := ResolvePath(""); got != "" {
		t.Errorf("ResolvePath(\"\") = %q, want empty", got)
	}
	if got := ResolvePath("/var/lib/metrika.db"); got != "/var/lib/metrika.db" {
		t.Errorf("ResolvePath(abs) = %q, want unchanged", got)
	}
	if got := ResolvePath("metrika.db"); got != "metrika.db" {
		t.Errorf("ResolvePath(rel) = %q, want metrika.db", got)
	}
	if got := ResolvePath("~/metrika.db"); got != filepath.Join(home, "metrika.db") {
		t.Errorf("ResolvePath(~/metrika.db) = %q, want %q", got, filepath.Join(home, "metrika.db"))
	}
	// ~ only expands when followed by another char (len > 1).
	if got := ResolvePath("~"); got != "~" {
		t.Errorf("ResolvePath(\"~\") = %q, want ~", got)
	}
}

func TestDefaultMetrikaURLs(t *testing.T) {
	base, logs := DefaultMetrikaURLs()
	if base != "https://api-metrika.yandex.ru" {
		t.Errorf("base = %q", base)
	}
	if logs != "https://logs.metrika.yandex.ru" {
		t.Errorf("logs = %q", logs)
	}
}

func TestParseAdminIDs(t *testing.T) {
	got, err := parseAdminIDs("1, 2 ,3,,4")
	if err != nil {
		t.Fatalf("parseAdminIDs: %v", err)
	}
	want := []int64{1, 2, 3, 4}
	if len(got) != len(want) {
		t.Fatalf("parseAdminIDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("parseAdminIDs[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestParseAdminIDs_ErrorOnGarbage(t *testing.T) {
	if _, err := parseAdminIDs("7,notanumber,9"); err == nil {
		t.Error("expected error for non-numeric admin id, got nil")
	}
}

func TestParseAdminIDs_Empty(t *testing.T) {
	got, err := parseAdminIDs("")
	if err != nil {
		t.Fatalf("parseAdminIDs(\"\"): %v", err)
	}
	if got != nil {
		t.Errorf("parseAdminIDs(\"\") = %v, want nil", got)
	}
}

func TestParsePositiveInt(t *testing.T) {
	if n, err := parsePositiveInt("42"); err != nil || n != 42 {
		t.Errorf("parsePositiveInt(42) = %d, %v", n, err)
	}
	if _, err := parsePositiveInt("x"); err == nil {
		t.Error("expected error for parsePositiveInt(x)")
	}
	if _, err := parsePositiveInt(""); err == nil {
		t.Error("expected error for parsePositiveInt(\"\")")
	}
}

func TestParseSignedInt(t *testing.T) {
	if n, err := parseSignedInt("-5"); err != nil || n != -5 {
		t.Errorf("parseSignedInt(-5) = %d, %v", n, err)
	}
	if _, err := parseSignedInt("abc"); err == nil {
		t.Error("expected error for parseSignedInt(abc)")
	}
}

func TestSplitNonEmpty(t *testing.T) {
	got := splitNonEmpty("1, 2 ,3,,4", ",")
	want := []string{"1", "2", "3", "4"}
	if len(got) != len(want) {
		t.Fatalf("splitNonEmpty = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitNonEmpty[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSplit(t *testing.T) {
	if got := split("", ","); got != nil {
		t.Errorf("split(\"\") = %v, want nil", got)
	}
	got := split("a,b,c", ",")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("split = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("split[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTrimSpace(t *testing.T) {
	if got := trimSpace("  x  "); got != "x" {
		t.Errorf("trimSpace = %q, want x", got)
	}
}
