package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, "telegram:\n  bot_token: abc\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Database != "metrika.db" {
		t.Errorf("database = %q", cfg.Database)
	}
	if cfg.VKTeams.BaseURL != DefaultVKTeamsURL {
		t.Errorf("vkteams base_url = %q, want the cloud endpoint", cfg.VKTeams.BaseURL)
	}
	if cfg.API.ListenAddr != ":8090" {
		t.Errorf("listen_addr = %q", cfg.API.ListenAddr)
	}
}

func TestLoadVKTeamsSection(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
vkteams:
  bot_token: "001.token"
  base_url: https://myteam.corp.example/bot/v1
  admin_ids:
    - admin@corp.example
    - ops@corp.example
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.VKTeams.BotToken != "001.token" {
		t.Errorf("bot_token = %q", cfg.VKTeams.BotToken)
	}
	if cfg.VKTeams.BaseURL != "https://myteam.corp.example/bot/v1" {
		t.Errorf("base_url = %q — on-premise endpoint was not kept", cfg.VKTeams.BaseURL)
	}
	if len(cfg.VKTeams.AdminIDs) != 2 || cfg.VKTeams.AdminIDs[0] != "admin@corp.example" {
		t.Errorf("admin_ids = %v", cfg.VKTeams.AdminIDs)
	}
}

func TestEnvOverridesWin(t *testing.T) {
	t.Setenv("METRIKA_VKTEAMS_TOKEN", "from-env")
	t.Setenv("METRIKA_VKTEAMS_ADMINS", "a@corp.ru, b@corp.ru")
	t.Setenv("METRIKA_ADMIN_IDS", "11,22")
	t.Setenv("METRIKA_API_ENABLED", "true")
	t.Setenv("METRIKA_API_TOKEN", "bearer-token")

	cfg, err := Load(writeConfig(t, "vkteams:\n  bot_token: from-file\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.VKTeams.BotToken != "from-env" {
		t.Errorf("bot_token = %q, want the env value", cfg.VKTeams.BotToken)
	}
	if len(cfg.VKTeams.AdminIDs) != 2 || cfg.VKTeams.AdminIDs[1] != "b@corp.ru" {
		t.Errorf("admin_ids = %v — spaces around the comma should be trimmed", cfg.VKTeams.AdminIDs)
	}
	if len(cfg.Telegram.AdminIDs) != 2 || cfg.Telegram.AdminIDs[0] != 11 {
		t.Errorf("telegram admin_ids = %v", cfg.Telegram.AdminIDs)
	}
	if !cfg.API.Enabled || cfg.API.AuthToken != "bearer-token" {
		t.Errorf("api = %+v", cfg.API)
	}
}

// METRIKA_DB_DIR relocates a relative path, but must not mangle an absolute one:
// joining them produced /data/var/lib/metrika-alert/metrika.db.
func TestDBDirLeavesAbsolutePathAlone(t *testing.T) {
	t.Setenv("METRIKA_DB_DIR", "/data")
	t.Setenv("METRIKA_DB_PATH", "/var/lib/metrika-alert/metrika.db")

	cfg, err := Load(writeConfig(t, "database: metrika.db\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database != "/var/lib/metrika-alert/metrika.db" {
		t.Errorf("database = %q, want the absolute path untouched", cfg.Database)
	}
}

func TestDBDirRelocatesRelativePath(t *testing.T) {
	t.Setenv("METRIKA_DB_DIR", "/data")

	cfg, err := Load(writeConfig(t, "database: metrika.db\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database != "/data/metrika.db" {
		t.Errorf("database = %q", cfg.Database)
	}
}

// The docs promise that 0 disables periodic reports.
func TestReportIntervalZeroDisablesReports(t *testing.T) {
	cfg, err := Load(writeConfig(t, "report_interval_hours: 0\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ReportHour != 0 {
		t.Errorf("report_interval_hours = %d, want 0 to survive as disabled", cfg.ReportHour)
	}
}

func TestBadAdminIDsDoNotOverrideConfig(t *testing.T) {
	t.Setenv("METRIKA_ADMIN_IDS", "not-a-number")

	cfg, err := Load(writeConfig(t, "telegram:\n  admin_ids:\n    - 99\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Telegram.AdminIDs) != 1 || cfg.Telegram.AdminIDs[0] != 99 {
		t.Errorf("admin_ids = %v — a malformed env value must not drop the configured admins", cfg.Telegram.AdminIDs)
	}
}

func TestLoadMissingFileFails(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("expected an error for a missing config file")
	}
}

// Earlier releases defaulted to hosts that do not serve the API. A config
// still carrying one must be corrected, or every call 404s.
func TestLegacyMetrikaHostIsCorrected(t *testing.T) {
	for _, legacy := range []string{
		"https://api-metrika.yandex.ru",
		"https://api-metrika.yandex.ru/",
		"https://logs.metrika.yandex.ru",
	} {
		cfg, err := Load(writeConfig(t, "metrika:\n  base_url: "+legacy+"\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Metrika.BaseURL != DefaultMetrikaURL {
			t.Errorf("base_url %q was kept as %q, want %q", legacy, cfg.Metrika.BaseURL, DefaultMetrikaURL)
		}
	}
}

func TestCustomMetrikaHostIsKept(t *testing.T) {
	const proxy = "https://metrika-proxy.corp.example"
	cfg, err := Load(writeConfig(t, "metrika:\n  base_url: "+proxy+"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Metrika.BaseURL != proxy {
		t.Errorf("base_url = %q, want the configured proxy kept", cfg.Metrika.BaseURL)
	}
}

func TestDefaultMetrikaHostIsTheDocumentedOne(t *testing.T) {
	cfg, err := Load(writeConfig(t, "database: x.db\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Metrika.BaseURL != "https://api-metrika.yandex.net" {
		t.Errorf("base_url = %q", cfg.Metrika.BaseURL)
	}
	if cfg.Metrika.LagHours < 24 {
		t.Errorf("lag_hours = %d — the Logs API refuses a window ending today", cfg.Metrika.LagHours)
	}
}

func TestLogsPacingOverrides(t *testing.T) {
	t.Setenv("METRIKA_LAG_HOURS", "48")
	t.Setenv("METRIKA_MAX_WINDOW_HOURS", "6")

	cfg, err := Load(writeConfig(t, "metrika:\n  lag_hours: 30\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Metrika.LagHours != 48 {
		t.Errorf("lag_hours = %d, want the env override", cfg.Metrika.LagHours)
	}
	if cfg.Metrika.MaxWindowHours != 6 {
		t.Errorf("max_window_hours = %d", cfg.Metrika.MaxWindowHours)
	}
}
