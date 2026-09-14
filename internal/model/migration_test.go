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

// Both used to dereference a nil pointer and crash the process: the reporter
// writes a counter-level snapshot on every cycle, and webhooks carry no chat ID.
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
		CounterID: c.ID, MonitorID: nil, Period: "hour",
		PeriodKey: "2026-09-12T11", TakenAt: time.Now(), Goals: map[string]int{"42": 7},
	}
	if err := db.CreateReportSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("counter-level snapshot: %v", err)
	}

	got, err := db.GetSnapshot(ctx, c.ID, nil, "hour", "2026-09-12T11")
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if got.MonitorID != nil {
		t.Errorf("MonitorID = %v, want nil", *got.MonitorID)
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
	if err := db.CreateMonitor(ctx, &PageMonitor{CounterID: c.ID, Name: "m", URLPattern: "/*", Metrics: []string{"visits"}, Enabled: true}); err != nil {
		t.Fatalf("CreateMonitor: %v", err)
	}
	trigger := &Trigger{CounterID: c.ID, Name: "t", Condition: "status_code == 500", Threshold: 1, Window: 5, Cooldown: 5, Enabled: true}
	if err := db.CreateTrigger(ctx, trigger); err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}
	if err := db.CreateAlert(ctx, &Alert{TriggerID: trigger.ID, CounterID: c.ID, Title: "a", EventCount: 1}); err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}

	if err := db.DeleteCounter(ctx, c.ID); err != nil {
		t.Fatalf("DeleteCounter: %v", err)
	}

	for _, table := range []string{"triggers", "page_monitors", "alerts", "report_snapshots"} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE counter_id = ?`, c.ID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s still holds %d orphaned row(s)", table, n)
		}
	}
}
