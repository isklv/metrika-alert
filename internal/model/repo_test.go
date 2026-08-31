package model

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// openTestDB opens a fresh SQLite DB in a temp dir and registers cleanup.
func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func i64p(v int64) *int64 { return &v }

func TestCounterCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	c := &Counter{Name: "Shop", CounterID: "12345678", OAuthToken: "tok", PollInterval: 30}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}
	if c.ID == 0 {
		t.Fatal("expected non-zero ID after create")
	}
	if c.CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be set")
	}

	got, err := db.GetCounter(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetCounter: %v", err)
	}
	if got.Name != "Shop" || got.CounterID != "12345678" || got.OAuthToken != "tok" || got.PollInterval != 30 {
		t.Errorf("GetCounter = %+v, want Shop/12345678/tok/30", got)
	}

	// Duplicate counter_id violates UNIQUE.
	if err := db.CreateCounter(ctx, &Counter{Name: "Dup", CounterID: "12345678", OAuthToken: "x"}); err == nil {
		t.Error("expected error for duplicate counter_id, got nil")
	}

	list, err := db.ListCounters(ctx)
	if err != nil {
		t.Fatalf("ListCounters: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListCounters = %d, want 1", len(list))
	}

	if err := db.DeleteCounter(ctx, c.ID); err != nil {
		t.Fatalf("DeleteCounter: %v", err)
	}
	if _, err := db.GetCounter(ctx, c.ID); err == nil {
		t.Error("expected error after delete, got nil")
	}
}

func TestGetCounter_NotFound(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.GetCounter(context.Background(), 999); err == nil {
		t.Error("expected not-found error, got nil")
	}
}

func TestMonitorCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	c := &Counter{Name: "Shop", CounterID: "1", OAuthToken: "t"}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}

	m := &PageMonitor{CounterID: c.ID, Name: "Checkout", URLPattern: "/checkout*", Metrics: []string{"visits", "goals"}, Enabled: true}
	if err := db.CreateMonitor(ctx, m); err != nil {
		t.Fatalf("CreateMonitor: %v", err)
	}
	if m.ID == 0 {
		t.Fatal("expected non-zero monitor ID")
	}

	list, err := db.ListMonitors(ctx, c.ID)
	if err != nil {
		t.Fatalf("ListMonitors: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListMonitors = %d, want 1", len(list))
	}
	if list[0].Name != "Checkout" || list[0].URLPattern != "/checkout*" {
		t.Errorf("monitor = %+v", list[0])
	}
	if len(list[0].Metrics) != 2 || list[0].Metrics[0] != "visits" || list[0].Metrics[1] != "goals" {
		t.Errorf("Metrics = %v, want [visits goals]", list[0].Metrics)
	}
	if !list[0].Enabled {
		t.Error("expected Enabled true")
	}

	// Update toggle.
	if err := db.UpdateMonitor(ctx, m.ID, false); err != nil {
		t.Fatalf("UpdateMonitor: %v", err)
	}
	list, _ = db.ListMonitors(ctx, c.ID)
	if list[0].Enabled {
		t.Error("expected Enabled false after update")
	}

	// ListMonitors for a different counter returns empty.
	other, _ := db.ListMonitors(ctx, c.ID+1)
	if len(other) != 0 {
		t.Errorf("ListMonitors(other) = %d, want 0", len(other))
	}

	if err := db.DeleteMonitor(ctx, m.ID); err != nil {
		t.Fatalf("DeleteMonitor: %v", err)
	}
	list, _ = db.ListMonitors(ctx, c.ID)
	if len(list) != 0 {
		t.Errorf("ListMonitors after delete = %d, want 0", len(list))
	}
}

func TestTriggerCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	c := &Counter{Name: "Shop", CounterID: "1", OAuthToken: "t"}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}
	m := &PageMonitor{CounterID: c.ID, Name: "M", URLPattern: "/x", Metrics: []string{"visits"}}
	if err := db.CreateMonitor(ctx, m); err != nil {
		t.Fatalf("CreateMonitor: %v", err)
	}

	// Trigger with monitor scope.
	tg := &Trigger{CounterID: c.ID, MonitorID: i64p(m.ID), Name: "Errors", Condition: "status_code == 500", Threshold: 3, Window: 15, Cooldown: 60, Enabled: true}
	if err := db.CreateTrigger(ctx, tg); err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}

	// Trigger without monitor scope.
	tg2 := &Trigger{CounterID: c.ID, Name: "Revenue", Condition: "revenue > 10000", Threshold: 1, Window: 30, Cooldown: 60, Enabled: true}
	if err := db.CreateTrigger(ctx, tg2); err != nil {
		t.Fatalf("CreateTrigger (no monitor): %v", err)
	}

	got, err := db.GetTrigger(ctx, tg.ID)
	if err != nil {
		t.Fatalf("GetTrigger: %v", err)
	}
	if got.MonitorID == nil || *got.MonitorID != m.ID {
		t.Errorf("MonitorID = %v, want %d", got.MonitorID, m.ID)
	}
	if got.Condition != "status_code == 500" || got.Threshold != 3 || got.Window != 15 || got.Cooldown != 60 {
		t.Errorf("trigger = %+v", got)
	}

	got2, err := db.GetTrigger(ctx, tg2.ID)
	if err != nil {
		t.Fatalf("GetTrigger(no monitor): %v", err)
	}
	if got2.MonitorID != nil {
		t.Errorf("MonitorID = %v, want nil", got2.MonitorID)
	}

	list, err := db.ListTriggers(ctx, c.ID)
	if err != nil {
		t.Fatalf("ListTriggers: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListTriggers = %d, want 2", len(list))
	}

	// ListTriggers for a non-existent counter errors.
	if _, err := db.ListTriggers(ctx, 99999); err == nil {
		t.Error("expected error for missing counter, got nil")
	}

	if err := db.UpdateTrigger(ctx, tg.ID, false); err != nil {
		t.Fatalf("UpdateTrigger: %v", err)
	}
	got, _ = db.GetTrigger(ctx, tg.ID)
	if got.Enabled {
		t.Error("expected Enabled false after update")
	}

	if err := db.DeleteTrigger(ctx, tg.ID); err != nil {
		t.Fatalf("DeleteTrigger: %v", err)
	}
	if _, err := db.GetTrigger(ctx, tg.ID); err == nil {
		t.Error("expected error after delete, got nil")
	}
}

func TestGetTrigger_NotFound(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.GetTrigger(context.Background(), 999); err == nil {
		t.Error("expected not-found error, got nil")
	}
}

func TestAlertActionCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Telegram action with chat id.
	tg := &AlertAction{Name: "telegram-123", Type: "telegram", ChatID: i64p(123)}
	if err := db.CreateAlertAction(ctx, tg); err != nil {
		t.Fatalf("CreateAlertAction (telegram): %v", err)
	}
	// Webhook action with nil ChatID — must not panic.
	wh := &AlertAction{Name: "webhook-x", Type: "webhook", URL: "https://example.com/hook"}
	if err := db.CreateAlertAction(ctx, wh); err != nil {
		t.Fatalf("CreateAlertAction (webhook): %v", err)
	}

	list, err := db.ListAlertActions(ctx)
	if err != nil {
		t.Fatalf("ListAlertActions: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListAlertActions = %d, want 2", len(list))
	}

	// Find each by type and verify round-trip of the nullable fields.
	var sawTG, sawWH bool
	for _, a := range list {
		switch a.Type {
		case "telegram":
			sawTG = true
			if a.ChatID == nil || *a.ChatID != 123 {
				t.Errorf("telegram ChatID = %v, want 123", a.ChatID)
			}
		case "webhook":
			sawWH = true
			if a.URL != "https://example.com/hook" {
				t.Errorf("webhook URL = %q", a.URL)
			}
			if a.ChatID != nil {
				t.Errorf("webhook ChatID = %v, want nil", a.ChatID)
			}
		}
	}
	if !sawTG || !sawWH {
		t.Errorf("sawTG=%v sawWH=%v, want both", sawTG, sawWH)
	}

	if err := db.DeleteAlertAction(ctx, tg.ID); err != nil {
		t.Fatalf("DeleteAlertAction: %v", err)
	}
	list, _ = db.ListAlertActions(ctx)
	if len(list) != 1 {
		t.Errorf("ListAlertActions after delete = %d, want 1", len(list))
	}
}

func TestAlertCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	c := &Counter{Name: "Shop", CounterID: "1", OAuthToken: "t"}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}
	tg := &Trigger{CounterID: c.ID, Name: "T", Condition: "x", Threshold: 1, Window: 30, Cooldown: 60, Enabled: true}
	if err := db.CreateTrigger(ctx, tg); err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}

	a1 := &Alert{TriggerID: tg.ID, CounterID: c.ID, Title: "First", Message: "msg1", EventCount: 5}
	a2 := &Alert{TriggerID: tg.ID, CounterID: c.ID, Title: "Second", Message: "msg2", EventCount: 7}
	if err := db.CreateAlert(ctx, a1); err != nil {
		t.Fatalf("CreateAlert 1: %v", err)
	}
	if err := db.CreateAlert(ctx, a2); err != nil {
		t.Fatalf("CreateAlert 2: %v", err)
	}

	all, err := db.RecentAlerts(ctx, 0, 10)
	if err != nil {
		t.Fatalf("RecentAlerts(all): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("RecentAlerts(all) = %d, want 2", len(all))
	}

	// Filter by counter.
	filtered, err := db.RecentAlerts(ctx, c.ID, 10)
	if err != nil {
		t.Fatalf("RecentAlerts(filtered): %v", err)
	}
	if len(filtered) != 2 {
		t.Fatalf("RecentAlerts(filtered) = %d, want 2", len(filtered))
	}
	for _, a := range filtered {
		if a.CounterID != c.ID {
			t.Errorf("alert counter = %d, want %d", a.CounterID, c.ID)
		}
	}

	// Limit respected.
	limited, err := db.RecentAlerts(ctx, c.ID, 1)
	if err != nil {
		t.Fatalf("RecentAlerts(limit): %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("RecentAlerts(limit=1) = %d, want 1", len(limited))
	}

	// Empty for unknown counter.
	empty, err := db.RecentAlerts(ctx, 99999, 10)
	if err != nil {
		t.Fatalf("RecentAlerts(empty): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("RecentAlerts(unknown) = %d, want 0", len(empty))
	}
}

func TestTriggerCooldown(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// No record yet.
	if fired, ok, err := db.TriggerFiredAt(ctx, 1); err != nil || ok {
		t.Fatalf("TriggerFiredAt (none) = %v, %v; want zero, false, nil", fired, ok)
	}

	if err := db.RecordTriggerFire(ctx, 1); err != nil {
		t.Fatalf("RecordTriggerFire: %v", err)
	}
	fired, ok, err := db.TriggerFiredAt(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("TriggerFiredAt (after) = %v, %v, %v; want set, true, nil", fired, ok, err)
	}
	if fired.IsZero() {
		t.Error("expected non-zero fired time")
	}

	// Upsert: record again without error.
	if err := db.RecordTriggerFire(ctx, 1); err != nil {
		t.Fatalf("RecordTriggerFire (again): %v", err)
	}
	if fired, ok, err := db.TriggerFiredAt(ctx, 1); err != nil || !ok {
		t.Fatalf("TriggerFiredAt (after upsert) = %v, %v, %v", fired, ok, err)
	}
}

func TestReportSnapshotCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	c := &Counter{Name: "Shop", CounterID: "1", OAuthToken: "t"}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}

	// Counter-level snapshot (nil MonitorID) — must not panic.
	s1 := &ReportSnapshot{
		CounterID: c.ID, MonitorID: nil, Period: "hour", PeriodKey: "2026-08-01T10",
		TakenAt: time.Now(), Visits: 100, UniqueVisits: 80, Bounces: 20,
		AvgDuration: 45.5, Depth: 2.3, Goals: map[string]int{"42": 7}, Revenue: 1200.5, Orders: 3,
	}
	if err := db.CreateReportSnapshot(ctx, s1); err != nil {
		t.Fatalf("CreateReportSnapshot (nil monitor): %v", err)
	}
	if s1.ID == 0 {
		t.Fatal("expected non-zero snapshot ID")
	}

	// Monitor-level snapshot.
	m := &PageMonitor{CounterID: c.ID, Name: "M", URLPattern: "/x", Metrics: []string{"visits"}}
	if err := db.CreateMonitor(ctx, m); err != nil {
		t.Fatalf("CreateMonitor: %v", err)
	}
	s2 := &ReportSnapshot{
		CounterID: c.ID, MonitorID: i64p(m.ID), Period: "hour", PeriodKey: "2026-08-01T11",
		TakenAt: time.Now(), Visits: 50, Goals: map[string]int{},
	}
	if err := db.CreateReportSnapshot(ctx, s2); err != nil {
		t.Fatalf("CreateReportSnapshot (monitor): %v", err)
	}

	// GetSnapshot counter-level (nil monitor).
	got, err := db.GetSnapshot(ctx, c.ID, nil, "hour", "2026-08-01T10")
	if err != nil {
		t.Fatalf("GetSnapshot (nil monitor): %v", err)
	}
	if got.Visits != 100 || got.UniqueVisits != 80 || got.Bounces != 20 {
		t.Errorf("snapshot = %+v", got)
	}
	if got.AvgDuration != 45.5 || got.Depth != 2.3 {
		t.Errorf("snapshot floats = %.2f/%.2f", got.AvgDuration, got.Depth)
	}
	if got.Goals["42"] != 7 {
		t.Errorf("Goals = %v, want {42:7}", got.Goals)
	}
	if got.Revenue != 1200.5 || got.Orders != 3 {
		t.Errorf("revenue/orders = %v/%d", got.Revenue, got.Orders)
	}
	if got.MonitorID != nil {
		t.Errorf("MonitorID = %v, want nil", got.MonitorID)
	}

	// GetSnapshot monitor-level.
	got2, err := db.GetSnapshot(ctx, c.ID, i64p(m.ID), "hour", "2026-08-01T11")
	if err != nil {
		t.Fatalf("GetSnapshot (monitor): %v", err)
	}
	if got2.Visits != 50 {
		t.Errorf("monitor snapshot visits = %d, want 50", got2.Visits)
	}
	if got2.MonitorID == nil || *got2.MonitorID != m.ID {
		t.Errorf("MonitorID = %v, want %d", got2.MonitorID, m.ID)
	}

	// ListSnapshots for the counter and hour period.
	list, err := db.ListSnapshots(ctx, c.ID, "hour", 20)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListSnapshots = %d, want 2", len(list))
	}

	// Limit respected.
	limited, _ := db.ListSnapshots(ctx, c.ID, "hour", 1)
	if len(limited) != 1 {
		t.Errorf("ListSnapshots(limit=1) = %d, want 1", len(limited))
	}

	// No match for a different period.
	none, err := db.ListSnapshots(ctx, c.ID, "day", 20)
	if err != nil {
		t.Fatalf("ListSnapshots(day): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("ListSnapshots(day) = %d, want 0", len(none))
	}
}
