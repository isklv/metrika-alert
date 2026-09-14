package engine

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

type alertRouter struct {
	alerts []*alertRecord
}

type alertRecord struct {
	counterID int64
	title     string
	message   string
}

func (r *alertRouter) Alert(ctx context.Context, counterID int64, title, message string) error {
	r.alerts = append(r.alerts, &alertRecord{counterID: counterID, title: title, message: message})
	return nil
}

func newTestEvaluator(t *testing.T) (*Evaluator, *model.DB, *alertRouter) {
	t.Helper()
	f, err := os.CreateTemp("", "metrika-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	db, err := model.OpenDB(f.Name())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	router := &alertRouter{}
	eval := NewEvaluator(db, router)
	return eval, db, router
}

func TestMatchCondition(t *testing.T) {
	tests := []struct {
		name string
		ev   *model.MetrikaEvent
		cond string
		want bool
	}{
		{"status equals", &model.MetrikaEvent{Status: "500"}, "status_code == 500", true},
		{"status not equal", &model.MetrikaEvent{Status: "200"}, "status_code == 500", false},
		{"not-equal op", &model.MetrikaEvent{Status: "404"}, "status_code != 200", true},
		{"url contains", &model.MetrikaEvent{PageURL: "/checkout/step1"}, "page_url contains /checkout", true},
		{"url not contains", &model.MetrikaEvent{PageURL: "/home"}, "page_url contains /checkout", false},
		{"revenue greater", &model.MetrikaEvent{Revenue: 1500.5}, "revenue > 1000", true},
		{"revenue less", &model.MetrikaEvent{Revenue: 500}, "revenue > 1000", false},
		{"goals contains", &model.MetrikaEvent{GoalsID: []string{"42", "99"}}, "goals_id contains 42", true},
		{"empty condition matches", &model.MetrikaEvent{PageURL: "/any"}, "", true},
		{"custom param", &model.MetrikaEvent{Params: map[string]string{"event_type": "purchase"}}, "event_type == purchase", true},
		{"missing field", &model.MetrikaEvent{Status: "200"}, "status_code == 500", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if matchCondition(tt.cond, tt.ev) != tt.want {
				t.Errorf("matchCondition(%q, %+v) = %v, want %v", tt.cond, tt.ev, !tt.want, tt.want)
			}
		})
	}
}

func TestPageMatches(t *testing.T) {
	tests := []struct {
		pat  string
		url  string
		want bool
	}{
		{"/checkout*", "/checkout/step1", true},
		{"/checkout*", "/checkout", true},
		{"/checkout*", "/home", false},
		{"*/api/*", "/v1/api/users", true},
		{"*/api/*", "/users/42", false},
		{"*", "/anything/here", true},
		{"", "/anything", true},
		{"/exact/path", "/exact/path", true},
		{"/exact/path", "/exact/other", false},
	}

	for _, tt := range tests {
		t.Run(tt.pat+" vs "+tt.url, func(t *testing.T) {
			if pageMatches(tt.pat, tt.url) != tt.want {
				t.Errorf("pageMatches(%q, %q) = %v, want %v", tt.pat, tt.url, !tt.want, tt.want)
			}
		})
	}
}

func TestSplitCondition(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"status_code == 500", []string{"status_code ", "==", " 500"}},
		{"page_url contains /checkout", []string{"page_url ", "contains", " /checkout"}},
		{"revenue > 10000", []string{"revenue ", ">", " 10000"}},
		{"revenue >= 500", []string{"revenue ", ">=", " 500"}},
		{"incomplete", []string{"incomplete"}},
	}

	for _, tt := range tests {
		got := splitCondition(tt.input)
		if len(got) != len(tt.want) || (len(got) == 3 && got[1] != tt.want[1]) {
			t.Errorf("split(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestEventField(t *testing.T) {
	ev := &model.MetrikaEvent{
		PageURL:  "/checkout",
		Title:    "Checkout Page",
		Status:   "500",
		Revenue:  99.9,
		OrderID:  "ORD-123",
		ClientID: "c-42",
		UserID:   "u-7",
		GoalsID:  []string{"42", "99"},
		Params:   map[string]string{"custom": "val"},
	}

	if eventField(ev, "page_url") != "/checkout" {
		t.Errorf("field page_url: %q", eventField(ev, "page_url"))
	}
	if eventField(ev, "url") != "/checkout" {
		t.Errorf("alias url: %q", eventField(ev, "url"))
	}
	if eventField(ev, "status_code") != "500" || eventField(ev, "status") != "500" {
		t.Errorf("status: %q / %q", eventField(ev, "status_code"), eventField(ev, "status"))
	}
	if eventField(ev, "revenue") != "99.90" {
		t.Errorf("revenue: %q", eventField(ev, "revenue"))
	}
	if eventField(ev, "goals_id") != "42,99" || eventField(ev, "goals") != "42,99" {
		t.Errorf("goals: %q / %q", eventField(ev, "goals_id"), eventField(ev, "goals"))
	}
	if eventField(ev, "custom") != "val" {
		t.Errorf("custom param: %q", eventField(ev, "custom"))
	}
	if eventField(ev, "nonexistent") != "" {
		t.Errorf("missing field: %q", eventField(ev, "nonexistent"))
	}
}

func TestEvaluatorAlertsOnThreshold(t *testing.T) {
	eval, db, router := newTestEvaluator(t)

	c := &model.Counter{
		Name:         "Test Shop",
		CounterID:    "12345",
		OAuthToken:   "test-token",
		PollInterval: 60,
	}
	if err := db.CreateCounter(context.Background(), c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	trig := &model.Trigger{
		CounterID: c.ID,
		Name:      "500 errors",
		Condition: "status_code == 500",
		Threshold: 3,
		Window:    60,
		Cooldown:  30,

		Enabled: true,
	}
	if err := db.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	now := time.Now()
	events := make([]*model.MetrikaEvent, 5)
	for i := 0; i < 5; i++ {
		events[i] = &model.MetrikaEvent{
			EventTime: now.Add(time.Duration(-i) * time.Minute),
			PageURL:   "/checkout",
			Title:     "Checkout page",
			Status:    "500",
		}
	}

	if err := eval.Evaluate(context.Background(), c.ID, events); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if len(router.alerts) != 1 {
		t.Fatalf("alerts delivered: got %d, want 1", len(router.alerts))
	}
	a := router.alerts[0]
	if a.counterID != c.ID {
		t.Errorf("alert counter_id: %d, want %d", a.counterID, c.ID)
	}
	if a.message == "" {
		t.Error("alert message empty")
	}
}

func TestEvaluatorDedupWithinCooldown(t *testing.T) {
	eval, db, router := newTestEvaluator(t)

	c := &model.Counter{
		Name:         "Test Shop",
		CounterID:    "12345",
		OAuthToken:   "test-token",
		PollInterval: 60,
	}
	if err := db.CreateCounter(context.Background(), c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	trig := &model.Trigger{
		CounterID: c.ID,
		Name:      "500 errors",
		Condition: "status_code == 500",
		Threshold: 2,
		Window:    60,
		Cooldown:  30,

		Enabled: true,
	}
	if err := db.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	now := time.Now()
	first := []*model.MetrikaEvent{
		{EventTime: now, Status: "500"},
		{EventTime: now.Add(-5 * time.Minute), Status: "500"},
	}
	if err := eval.Evaluate(context.Background(), c.ID, first); err != nil {
		t.Fatalf("first evaluate: %v", err)
	}

	second := []*model.MetrikaEvent{
		{EventTime: now.Add(-10 * time.Minute), Status: "500"},
		{EventTime: now.Add(-11 * time.Minute), Status: "500"},
	}
	if err := eval.Evaluate(context.Background(), c.ID, second); err != nil {
		t.Fatalf("second evaluate: %v", err)
	}

	if len(router.alerts) != 1 {
		t.Errorf("alerts delivered: got %d, want 1 (deduped by cooldown)", len(router.alerts))
	}
}

func TestEvaluatorNoAlertBelowThreshold(t *testing.T) {
	eval, db, router := newTestEvaluator(t)

	c := &model.Counter{
		Name:         "Test Shop",
		CounterID:    "12345",
		OAuthToken:   "test-token",
		PollInterval: 60,
	}
	if err := db.CreateCounter(context.Background(), c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	trig := &model.Trigger{
		CounterID: c.ID,
		Name:      "500 errors",
		Condition: "status_code == 500",
		Threshold: 5,
		Window:    60,
		Cooldown:  30,

		Enabled: true,
	}
	if err := db.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	now := time.Now()
	events := make([]*model.MetrikaEvent, 3)
	for i := 0; i < 3; i++ {
		events[i] = &model.MetrikaEvent{
			EventTime: now.Add(time.Duration(-i) * time.Minute),
			Status:    "500",
		}
	}

	if err := eval.Evaluate(context.Background(), c.ID, events); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if len(router.alerts) != 0 {
		t.Errorf("alerts delivered: got %d, want 0 (3 events < threshold 5)", len(router.alerts))
	}
}

func TestEvaluatorDisabledTriggerIgnored(t *testing.T) {
	eval, db, router := newTestEvaluator(t)

	c := &model.Counter{
		Name:         "Test Shop",
		CounterID:    "12345",
		OAuthToken:   "test-token",
		PollInterval: 60,
	}
	if err := db.CreateCounter(context.Background(), c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	trig := &model.Trigger{
		CounterID: c.ID,
		Name:      "500 errors",
		Condition: "status_code == 500",
		Threshold: 1,
		Window:    60,
		Cooldown:  30,
		Enabled:   false,
	}
	if err := db.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	events := []*model.MetrikaEvent{{Status: "500"}}
	if err := eval.Evaluate(context.Background(), c.ID, events); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if len(router.alerts) != 0 {
		t.Errorf("alerts delivered: got %d, want 0 (trigger disabled)", len(router.alerts))
	}
}

func TestEvaluatorMonitorScopedTrigger(t *testing.T) {
	eval, db, router := newTestEvaluator(t)

	c := &model.Counter{
		Name:         "Test Shop",
		CounterID:    "12345",
		OAuthToken:   "test-token",
		PollInterval: 60,
	}
	if err := db.CreateCounter(context.Background(), c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	monitor := &model.PageMonitor{
		CounterID:  c.ID,
		Name:       "Checkout",
		URLPattern: "/checkout*",
		Metrics:    []string{"visits", "bounces"},
		Enabled:    true,
	}
	if err := db.CreateMonitor(context.Background(), monitor); err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	monitorID := monitor.ID
	trig := &model.Trigger{
		CounterID: c.ID,
		MonitorID: &monitorID,
		Name:      "Checkout 500s",
		Condition: "status_code == 500",
		Threshold: 2,
		Window:    60,
		Cooldown:  30,

		Enabled: true,
	}
	if err := db.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	// Debug diagnostics.
	trigs, _ := db.ListTriggers(context.Background(), c.ID)
	mons, _ := db.ListMonitors(context.Background(), c.ID)
	fmt.Printf("diagnose: triggers=%d monitors=%d\n", len(trigs), len(mons))
	for _, tr := range trigs {
		mid := "<nil>"
		if tr.MonitorID != nil {
			mid = fmt.Sprintf("%d", *tr.MonitorID)
		}
		fmt.Printf("  trigger id=%d monitor_id=%s enabled=%v\n", tr.ID, mid, tr.Enabled)
	}
	for _, m := range mons {
		fmt.Printf("  monitor id=%d pattern=%q enabled=%v\n", m.ID, m.URLPattern, m.Enabled)
	}

	now := time.Now()
	events := []*model.MetrikaEvent{
		{EventTime: now, PageURL: "/checkout/step1", Status: "500"},
		{EventTime: now.Add(-2 * time.Minute), PageURL: "/checkout/step2", Status: "500"},
		{EventTime: now.Add(-3 * time.Minute), PageURL: "/home", Status: "500"}, // outside monitor
	}

	if err := eval.Evaluate(context.Background(), c.ID, events); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if len(router.alerts) != 1 {
		t.Fatalf("alerts delivered: got %d, want 1", len(router.alerts))
	}
	// Alert message should mention 2 matched events (only checkout ones).
	msg := router.alerts[0].message
	if !containsStr(msg, "2") || !containsStr(msg, "threshold: 2") {
		t.Errorf("alert message missing event count: %q", msg)
	}
}

func TestEvaluatorMultipleTriggersOneCounter(t *testing.T) {
	eval, db, router := newTestEvaluator(t)

	c := &model.Counter{
		Name:         "Test Shop",
		CounterID:    "12345",
		OAuthToken:   "test-token",
		PollInterval: 60,
	}
	if err := db.CreateCounter(context.Background(), c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	trig1 := &model.Trigger{
		CounterID: c.ID,
		Name:      "500 errors",
		Condition: "status_code == 500",
		Threshold: 2,
		Window:    60,
		Cooldown:  30,

		Enabled: true,
	}
	trig2 := &model.Trigger{
		CounterID: c.ID,
		Name:      "Checkout visits",
		Condition: "page_url contains /checkout",
		Threshold: 50,
		Window:    1440,
		Cooldown:  60,

		Enabled: true,
	}
	if err := db.CreateTrigger(context.Background(), trig1); err != nil {
		t.Fatalf("create trigger1: %v", err)
	}
	if err := db.CreateTrigger(context.Background(), trig2); err != nil {
		t.Fatalf("create trigger2: %v", err)
	}

	now := time.Now()
	events := make([]*model.MetrikaEvent, 0, 52)
	for i := 0; i < 3; i++ {
		events = append(events, &model.MetrikaEvent{
			EventTime: now.Add(time.Duration(-i) * time.Minute),
			PageURL:   "/checkout",
			Status:    "500",
		})
	}
	for i := 0; i < 50; i++ {
		events = append(events, &model.MetrikaEvent{
			EventTime: now.Add(time.Duration(-10-i) * time.Minute),
			PageURL:   "/checkout/step1",
			Status:    "200",
		})
	}

	if err := eval.Evaluate(context.Background(), c.ID, events); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	// Both triggers should fire.
	if len(router.alerts) != 2 {
		t.Errorf("alerts delivered: got %d, want 2 (one per trigger)", len(router.alerts))
		for i, a := range router.alerts {
			t.Logf("  alert %d: counter=%d title=%q", i+1, a.counterID, a.title)
		}
	}
}

func TestEvaluatorOldEventsOutsideWindowIgnored(t *testing.T) {
	eval, db, router := newTestEvaluator(t)

	c := &model.Counter{
		Name:         "Test Shop",
		CounterID:    "12345",
		OAuthToken:   "test-token",
		PollInterval: 60,
	}
	if err := db.CreateCounter(context.Background(), c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	trig := &model.Trigger{
		CounterID: c.ID,
		Name:      "500 errors",
		Condition: "status_code == 500",
		Threshold: 1,
		Window:    5, // only last 5 minutes
		Cooldown:  30,

		Enabled: true,
	}
	if err := db.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	// Events from 10 minutes ago — outside the 5-min window.
	events := []*model.MetrikaEvent{
		{EventTime: time.Now().Add(-10 * time.Minute), Status: "500"},
	}

	if err := eval.Evaluate(context.Background(), c.ID, events); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if len(router.alerts) != 0 {
		t.Errorf("alerts delivered: got %d, want 0 (events outside window)", len(router.alerts))
	}
}

func TestEvaluatorRevenueCondition(t *testing.T) {
	eval, db, router := newTestEvaluator(t)

	c := &model.Counter{
		Name:         "Test Shop",
		CounterID:    "12345",
		OAuthToken:   "test-token",
		PollInterval: 60,
	}
	if err := db.CreateCounter(context.Background(), c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	trig := &model.Trigger{
		CounterID: c.ID,
		Name:      "High revenue",
		Condition: "revenue > 1000",
		Threshold: 2,
		Window:    60,
		Cooldown:  30,

		Enabled: true,
	}
	if err := db.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	// Revenue events with no window time → use explicit times inside the window.
	now := time.Now()
	events := []*model.MetrikaEvent{
		{EventTime: now, Revenue: 1500},
		{EventTime: now.Add(-5 * time.Minute), Revenue: 2000},
		{EventTime: now.Add(-10 * time.Minute), Revenue: 500}, // below threshold — should not count
	}

	if err := eval.Evaluate(context.Background(), c.ID, events); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if len(router.alerts) != 1 {
		t.Errorf("alerts delivered: got %d, want 1", len(router.alerts))
	}
}

func TestEvaluatorAlertRecordedInDB(t *testing.T) {
	eval, db, _ := newTestEvaluator(t)

	c := &model.Counter{
		Name:         "Test Shop",
		CounterID:    "12345",
		OAuthToken:   "test-token",
		PollInterval: 60,
	}
	if err := db.CreateCounter(context.Background(), c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	trig := &model.Trigger{
		CounterID: c.ID,
		Name:      "500 errors",
		Condition: "status_code == 500",
		Threshold: 1,
		Window:    60,
		Cooldown:  30,

		Enabled: true,
	}
	if err := db.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	events := []*model.MetrikaEvent{{EventTime: time.Now(), Status: "500"}}
	if err := eval.Evaluate(context.Background(), c.ID, events); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	alerts, err := db.RecentAlerts(context.Background(), c.ID, 10)
	if err != nil || len(alerts) != 1 {
		t.Errorf("alerts in db: got %d, want 1; err=%v", len(alerts), err)
		return
	}
	if alerts[0].TriggerID != trig.ID || alerts[0].CounterID != c.ID {
		t.Errorf("alert record: trigger=%d counter=%d", alerts[0].TriggerID, alerts[0].CounterID)
	}
}

func TestMatchConditionWithFloatRevenue(t *testing.T) {
	ev := &model.MetrikaEvent{Revenue: 99.9}
	if !matchCondition("revenue > 50", ev) {
		t.Error("revenue 99.9 should be > 50")
	}
	if matchCondition("revenue > 200", ev) {
		t.Error("revenue 99.9 should not be > 200")
	}
	if !matchCondition("revenue < 200", ev) {
		t.Error("revenue 99.9 should be < 200")
	}
	if matchCondition("revenue >= 200", ev) {
		t.Error("revenue 99.9 should not be >= 200")
	}
}

func TestMatchConditionWithIntRevenue(t *testing.T) {
	ev := &model.MetrikaEvent{Revenue: 500}
	if !matchCondition("revenue > 400", ev) {
		t.Error("revenue 500 should be > 400")
	}
}

func TestMatchConditionWithJSONNumber(t *testing.T) {
	ev := &model.MetrikaEvent{Revenue: 750}
	if !matchCondition("revenue > 700", ev) {
		t.Error("revenue 750 should be > 700")
	}
}

func TestEvaluatorLogsErrorOnAlertDeliveryFailure(t *testing.T) {
	// The evaluator logs errors but continues — we verify no panic.
	eval, db, _ := newTestEvaluator(t)

	c := &model.Counter{
		Name:         "Test Shop",
		CounterID:    "12345",
		OAuthToken:   "test-token",
		PollInterval: 60,
	}
	if err := db.CreateCounter(context.Background(), c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	trig := &model.Trigger{
		CounterID: c.ID,
		Name:      "500 errors",
		Condition: "status_code == 500",
		Threshold: 1,
		Window:    60,
		Cooldown:  30,

		Enabled: true,
	}
	if err := db.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	events := []*model.MetrikaEvent{{Status: "500"}}
	// Should not panic even with a failing router.
	err := eval.Evaluate(context.Background(), c.ID, events)
	if err != nil {
		t.Errorf("evaluate failed: %v", err)
	}
}

func TestEvaluatorWithEmptyEvents(t *testing.T) {
	eval, db, router := newTestEvaluator(t)

	c := &model.Counter{
		Name:         "Test Shop",
		CounterID:    "12345",
		OAuthToken:   "test-token",
		PollInterval: 60,
	}
	if err := db.CreateCounter(context.Background(), c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	trig := &model.Trigger{
		CounterID: c.ID,
		Name:      "500 errors",
		Condition: "status_code == 500",
		Threshold: 1,
		Window:    60,
		Cooldown:  30,

		Enabled: true,
	}
	if err := db.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	// Empty event batch — should not alert.
	if err := eval.Evaluate(context.Background(), c.ID, []*model.MetrikaEvent{}); err != nil {
		t.Fatalf("evaluate empty: %v", err)
	}

	if len(router.alerts) != 0 {
		t.Errorf("alerts delivered: got %d, want 0 (no events)", len(router.alerts))
	}
}

func TestEvaluatorWithNonExistentCounter(t *testing.T) {
	eval, _, router := newTestEvaluator(t)

	events := []*model.MetrikaEvent{{Status: "500"}}
	err := eval.Evaluate(context.Background(), 9999, events)
	if err == nil {
		t.Error("evaluate with non-existent counter should fail")
	}
	if len(router.alerts) != 0 {
		t.Errorf("alerts delivered: got %d, want 0", len(router.alerts))
	}
}

func TestEvaluatorWithInvalidCondition(t *testing.T) {
	eval, db, router := newTestEvaluator(t)

	c := &model.Counter{
		Name:         "Test Shop",
		CounterID:    "12345",
		OAuthToken:   "test-token",
		PollInterval: 60,
	}
	if err := db.CreateCounter(context.Background(), c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	trig := &model.Trigger{
		CounterID: c.ID,
		Name:      "Bad condition",
		Condition: "invalid condition without operator",
		Threshold: 1,
		Window:    60,
		Cooldown:  30,

		Enabled: true,
	}
	if err := db.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	events := []*model.MetrikaEvent{{Status: "500"}}
	if err := eval.Evaluate(context.Background(), c.ID, events); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	// Invalid condition should not match anything, so no alert.
	if len(router.alerts) != 0 {
		t.Errorf("alerts delivered: got %d, want 0 (invalid condition)", len(router.alerts))
	}
}

func TestEvaluatorWithRevenueThreshold(t *testing.T) {
	eval, db, router := newTestEvaluator(t)

	c := &model.Counter{
		Name:         "Test Shop",
		CounterID:    "12345",
		OAuthToken:   "test-token",
		PollInterval: 60,
	}
	if err := db.CreateCounter(context.Background(), c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	trig := &model.Trigger{
		CounterID: c.ID,
		Name:      "High revenue events",
		Condition: "revenue > 1000",
		Threshold: 2,
		Window:    60,
		Cooldown:  30,

		Enabled: true,
	}
	if err := db.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	events := []*model.MetrikaEvent{
		{EventTime: time.Now(), Revenue: 1500},
		{EventTime: time.Now().Add(-5 * time.Minute), Revenue: 2000},
		{EventTime: time.Now().Add(-10 * time.Minute), Revenue: 500}, // below threshold
	}

	if err := eval.Evaluate(context.Background(), c.ID, events); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if len(router.alerts) != 1 {
		t.Errorf("alerts delivered: got %d, want 1", len(router.alerts))
	}
}

func TestEvaluatorWithRevenueThresholdFromFloat64(t *testing.T) {
	eval, db, router := newTestEvaluator(t)

	c := &model.Counter{
		Name:         "Test Shop",
		CounterID:    "12345",
		OAuthToken:   "test-token",
		PollInterval: 60,
	}
	if err := db.CreateCounter(context.Background(), c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	trig := &model.Trigger{
		CounterID: c.ID,
		Name:      "High revenue events",
		Condition: "revenue > 1000",
		Threshold: 1,
		Window:    60,
		Cooldown:  30,

		Enabled: true,
	}
	if err := db.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	events := []*model.MetrikaEvent{{EventTime: time.Now(), Revenue: float64(1500.5)}}

	if err := eval.Evaluate(context.Background(), c.ID, events); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if len(router.alerts) != 1 {
		t.Errorf("alerts delivered: got %d, want 1", len(router.alerts))
	}
}

func TestEvaluatorWithRevenueThresholdFromInt(t *testing.T) {
	eval, db, router := newTestEvaluator(t)

	c := &model.Counter{
		Name:         "Test Shop",
		CounterID:    "12345",
		OAuthToken:   "test-token",
		PollInterval: 60,
	}
	if err := db.CreateCounter(context.Background(), c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	trig := &model.Trigger{
		CounterID: c.ID,
		Name:      "High revenue events",
		Condition: "revenue > 1000",
		Threshold: 1,
		Window:    60,
		Cooldown:  30,

		Enabled: true,
	}
	if err := db.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	events := []*model.MetrikaEvent{{Revenue: 500.0}}

	if err := eval.Evaluate(context.Background(), c.ID, events); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	// Revenue 500 should not trigger "revenue > 1000"
	if len(router.alerts) != 0 {
		t.Errorf("alerts delivered: got %d, want 0 (revenue 500 < 1000)", len(router.alerts))
	}
}

func TestEvaluatorWithRevenueThresholdFromJSONNumber(t *testing.T) {
	eval, db, router := newTestEvaluator(t)

	c := &model.Counter{
		Name:         "Test Shop",
		CounterID:    "12345",
		OAuthToken:   "test-token",
		PollInterval: 60,
	}
	if err := db.CreateCounter(context.Background(), c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	trig := &model.Trigger{
		CounterID: c.ID,
		Name:      "High revenue events",
		Condition: "revenue > 1000",
		Threshold: 1,
		Window:    60,
		Cooldown:  30,

		Enabled: true,
	}
	if err := db.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	events := []*model.MetrikaEvent{{Revenue: 750.0}}

	if err := eval.Evaluate(context.Background(), c.ID, events); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	// Revenue 750 should not trigger "revenue > 1000"
	if len(router.alerts) != 0 {
		t.Errorf("alerts delivered: got %d, want 0 (revenue 750 < 1000)", len(router.alerts))
	}
}

// jsonNumber simulates json.Number from API responses.
type jsonNumber float64

func (n jsonNumber) Int64() (int64, error) {
	return int64(n), nil
}

func (n jsonNumber) Float64() (float64, error) {
	return float64(n), nil
}

// containsStr checks if s contains any of the substrings.
func containsStr(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if !containsAny(s, sub) {
			return false
		}
	}
	return true
}

func containsAny(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// eventToString converts an event to a string for display.
func eventToString(ev *model.MetrikaEvent) string {
	var b strings.Builder
	b.WriteString("Event: ")
	b.WriteString(ev.PageURL)
	b.WriteString(" (")
	b.WriteString(ev.Title)
	b.WriteString(")")
	if ev.Revenue > 0 {
		b.WriteString(", revenue: ")
		b.WriteString(strconv.FormatFloat(ev.Revenue, 'f', 2, 64))
	}
	return b.String()
}

// alertToString converts an alert record to a string for display.
func alertToString(a *alertRecord) string {
	var b strings.Builder
	b.WriteString("Alert: ")
	b.WriteString(a.title)
	b.WriteString(" (counter: ")
	b.WriteString(strconv.FormatInt(a.counterID, 10))
	b.WriteString(")")
	return b.String()
}

// renderAlertMessage formats an alert message for display.
func renderAlertMessage(t *model.Trigger, matched int) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Trigger: %s\n", t.Name))
	b.WriteString(fmt.Sprintf("Events matched: %d (threshold: %d)\n", matched, t.Threshold))
	b.WriteString(fmt.Sprintf("Window: %d min | Cooldown: %d min\n", t.Window, t.Cooldown))
	b.WriteString(fmt.Sprintf("Condition: `%s`", t.Condition))
	return b.String()
}
