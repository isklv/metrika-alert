package model

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// legacySchema is the alert_actions table as shipped before VK Teams existed.
const legacySchema = `
CREATE TABLE alert_actions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	type TEXT NOT NULL CHECK(type IN ('telegram','webhook')),
	chat_id INTEGER,
	url TEXT,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);`

// A database created by an earlier release must keep its rows and accept the
// new destination type — the CHECK constraint would otherwise reject it.
func TestMigrationUpgradesLegacyAlertActions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := raw.Exec(legacySchema); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO alert_actions (id, name, type, chat_id, url) VALUES (1, 'ops', 'telegram', -100500, NULL), (2, 'hook', 'webhook', NULL, 'https://ops.example/hook')`); err != nil {
		t.Fatalf("seed legacy rows: %v", err)
	}
	raw.Close()

	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB on a legacy database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	actions, err := db.ListAlertActions(ctx)
	if err != nil {
		t.Fatalf("ListAlertActions: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("got %d rows after migration, want the 2 that existed", len(actions))
	}

	// IDs are preserved, so anything referencing them still resolves.
	if actions[0].ID != 1 || actions[0].Type != "telegram" || actions[0].ChatID == nil || *actions[0].ChatID != -100500 {
		t.Errorf("telegram row after migration: %+v", actions[0])
	}
	// chat_id is backfilled into target so both columns agree.
	if actions[0].Target != "-100500" {
		t.Errorf("target = %q, want the chat_id backfilled", actions[0].Target)
	}
	if actions[1].URL != "https://ops.example/hook" {
		t.Errorf("webhook row after migration: %+v", actions[1])
	}

	if err := db.CreateAlertAction(ctx, &AlertAction{Name: "vk", Type: "vkteams", Target: "team@corp.ru"}); err != nil {
		t.Fatalf("legacy database still rejects vkteams: %v", err)
	}
}

// Opening an already-current database twice must not rebuild anything.
func TestMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.db")

	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if err := db.CreateAlertAction(context.Background(), &AlertAction{Name: "vk", Type: "vkteams", Target: "a@b.c"}); err != nil {
		t.Fatalf("CreateAlertAction: %v", err)
	}
	db.Close()

	db, err = OpenDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()

	actions, err := db.ListAlertActions(context.Background())
	if err != nil {
		t.Fatalf("ListAlertActions: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("got %d rows after reopening, want 1", len(actions))
	}
}

// A webhook action carries no chat ID; dereferencing it used to crash the process.
func TestOptionalIDsAreStoredAsNull(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if err := db.CreateAlertAction(ctx, &AlertAction{Name: "hook", Type: "webhook", URL: "https://x.example"}); err != nil {
		t.Fatalf("webhook action with no chat_id: %v", err)
	}

	c := &Counter{Name: "n", CounterID: "1", OAuthToken: "t", PollInterval: 60}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}

	snapshot := &ReportSnapshot{
		CounterID: c.ID, Period: "hour",
		PeriodKey: "2026-09-12T11", TakenAt: time.Now(), Goals: map[string]int{"42": 7},
	}
	if err := db.CreateReportSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("counter-level snapshot: %v", err)
	}

	got, err := db.GetSnapshot(ctx, c.ID, "hour", "2026-09-12T11")
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if got.Goals["42"] != 7 {
		t.Errorf("goals = %v", got.Goals)
	}
}

// Deleting a counter used to leave its triggers, monitors and history behind.
func TestDeleteCounterRemovesDependentRows(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	c := &Counter{Name: "n", CounterID: "1", OAuthToken: "t", PollInterval: 60}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}
	trigger := &Trigger{CounterID: c.ID, Name: "t", Metric: "visits", Direction: "drop", DeviationPct: 30, MinBaseline: 10, BaselineWeeks: 4, Cooldown: 5, Enabled: true}
	if err := db.CreateTrigger(ctx, trigger); err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}
	if err := db.CreateAlert(ctx, &Alert{TriggerID: trigger.ID, CounterID: c.ID, Title: "a", EventCount: 1}); err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}

	if err := db.DeleteCounter(ctx, c.ID); err != nil {
		t.Fatalf("DeleteCounter: %v", err)
	}

	for _, table := range []string{"triggers", "alerts", "report_snapshots"} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE counter_id = ?`, c.ID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s still holds %d orphaned row(s)", table, n)
		}
	}
}

// legacyEngineSchema is the shape shipped while alerts were driven by the Logs
// API: per-event trigger conditions, page monitors and export tracking.
const legacyEngineSchema = `
CREATE TABLE counters (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	counter_id TEXT UNIQUE NOT NULL,
	oauth_token TEXT NOT NULL,
	poll_interval_minutes INTEGER NOT NULL DEFAULT 60,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE page_monitors (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	counter_id INTEGER NOT NULL,
	name TEXT NOT NULL,
	url_pattern TEXT NOT NULL,
	metrics TEXT NOT NULL DEFAULT 'visits',
	enabled BOOLEAN NOT NULL DEFAULT 1,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE triggers (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	counter_id INTEGER NOT NULL,
	monitor_id INTEGER,
	name TEXT NOT NULL,
	condition TEXT NOT NULL,
	threshold INTEGER NOT NULL DEFAULT 1,
	window_minutes INTEGER NOT NULL DEFAULT 30,
	cooldown_minutes INTEGER NOT NULL DEFAULT 60,
	enabled BOOLEAN NOT NULL DEFAULT 1,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE trigger_cooldowns (
	trigger_id INTEGER PRIMARY KEY,
	last_fired_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE log_requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	counter_id INTEGER NOT NULL,
	request_id INTEGER NOT NULL,
	date1 DATETIME NOT NULL,
	date2 DATETIME NOT NULL,
	status TEXT NOT NULL,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);`

// A database from the Logs API era must open, keep its counters, and come out
// with metric rules — the old per-event conditions cannot be expressed as
// hourly metrics, so they go.
func TestMigrationFromPerEventEngine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-engine.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := raw.Exec(legacyEngineSchema + legacySchema); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO counters (id, name, counter_id, oauth_token) VALUES (1, 'Магазин', '12345678', 'y0_tok');
		 INSERT INTO triggers (counter_id, name, condition) VALUES (1, 'Ошибки', 'status_code == 500');
		 INSERT INTO page_monitors (counter_id, name, url_pattern) VALUES (1, 'Чекаут', '/checkout*');
		 INSERT INTO log_requests (counter_id, request_id, date1, date2, status) VALUES (1, 42, '2026-09-01', '2026-09-02', 'created');`); err != nil {
		t.Fatalf("seed legacy rows: %v", err)
	}
	raw.Close()

	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB on a Logs API era database: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// Counters and their tokens survive: they are the part worth keeping.
	counters, err := db.ListCounters(ctx)
	if err != nil {
		t.Fatalf("ListCounters: %v", err)
	}
	if len(counters) != 1 || counters[0].CounterID != "12345678" || counters[0].OAuthToken != "y0_tok" {
		t.Fatalf("counters after migration: %+v", counters)
	}
	if counters[0].LastHourChecked != nil {
		t.Error("a migrated counter should start with no hour cursor")
	}

	// The untranslatable event conditions are gone, and the table now holds rules.
	triggers, err := db.ListTriggers(ctx, 1)
	if err != nil {
		t.Fatalf("ListTriggers: %v", err)
	}
	if len(triggers) != 0 {
		t.Errorf("event-condition triggers survived: %+v", triggers)
	}
	if err := db.CreateTrigger(ctx, &Trigger{
		CounterID: 1, Name: "Визиты", Metric: "visits", Direction: "drop",
		DeviationPct: 40, MinBaseline: 10, BaselineWeeks: 4, Cooldown: 180, Enabled: true,
	}); err != nil {
		t.Fatalf("migrated database rejects a metric rule: %v", err)
	}

	// The structures that served only the retired engine are dropped.
	for _, table := range []string{"page_monitors", "log_requests"} {
		var name string
		err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != sql.ErrNoRows {
			t.Errorf("%s still exists after migration", table)
		}
	}
}

// Opening a current database must not disturb the rules it already holds.
func TestMigrationLeavesCurrentRulesAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current-rules.db")

	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	ctx := context.Background()
	if err := db.CreateCounter(ctx, &Counter{Name: "n", CounterID: "1", OAuthToken: "t", PollInterval: 60}); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}
	if err := db.CreateTrigger(ctx, &Trigger{
		CounterID: 1, Name: "Визиты", Metric: "visits", Direction: "drop",
		DeviationPct: 40, MinBaseline: 10, BaselineWeeks: 4, Cooldown: 180, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}
	db.Close()

	db, err = OpenDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()

	triggers, err := db.ListTriggers(ctx, 1)
	if err != nil {
		t.Fatalf("ListTriggers: %v", err)
	}
	if len(triggers) != 1 || triggers[0].Metric != "visits" || triggers[0].DeviationPct != 40 {
		t.Errorf("rules after reopening: %+v", triggers)
	}
}

func TestHourCursorRoundTrips(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "cursor.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	c := &Counter{Name: "n", CounterID: "1", OAuthToken: "t", PollInterval: 60}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}
	if got, _ := db.GetCounter(ctx, c.ID); got.LastHourChecked != nil {
		t.Error("a new counter should have no cursor")
	}

	hour := time.Date(2026, 9, 8, 15, 0, 0, 0, time.Local)
	if err := db.SetLastHourChecked(ctx, c.ID, hour); err != nil {
		t.Fatalf("SetLastHourChecked: %v", err)
	}

	got, err := db.GetCounter(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetCounter: %v", err)
	}
	if got.LastHourChecked == nil || !got.LastHourChecked.Equal(hour) {
		t.Errorf("cursor = %v, want %v", got.LastHourChecked, hour)
	}
}
