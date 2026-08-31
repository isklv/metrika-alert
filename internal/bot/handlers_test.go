package bot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/isklv/metrika-alert/internal/model"
)

// newFakeAPI returns a *tgbotapi.BotAPI that POSTs to an httptest server
// answering every request with a successful Telegram API envelope. The
// constructor calls getMe, so that endpoint returns a User; the rest return
// a Message.
func newFakeAPI(t *testing.T) *tgbotapi.BotAPI {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/getMe") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"result": map[string]any{"id": 1, "is_bot": true, "first_name": "Test", "username": "test_bot"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 1, "chat": map[string]any{"id": 1}},
		})
	}))
	t.Cleanup(ts.Close)
	// MakeRequest builds the URL via fmt.Sprintf(endpoint, token, method),
	// so the endpoint must carry two %s placeholders.
	api, err := tgbotapi.NewBotAPIWithClient("test-token", ts.URL+"/%s/%s", ts.Client())
	if err != nil {
		t.Fatalf("new bot api: %v", err)
	}
	return api
}

func newTestBot(t *testing.T) (*Bot, *model.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := model.OpenDB(filepath.Join(dir, "bot.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	api := newFakeAPI(t)
	return NewBot(api, db, []int64{1001}), db
}

func msg(text string, from int64) tgbotapi.Message {
	m := tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 42},
		From: &tgbotapi.User{ID: from},
		Text: text,
	}
	// IsCommand() requires a bot_command entity at offset 0 covering the
	// command name (leading slash included).
	if strings.HasPrefix(text, "/") {
		length := len(text)
		if i := strings.Index(text, " "); i != -1 {
			length = i
		}
		m.Entities = []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: length},
		}
	}
	return m
}

func TestIsAdmin(t *testing.T) {
	b, _ := newTestBot(t)
	if !b.IsAdmin(1001) {
		t.Error("1001 should be admin")
	}
	if b.IsAdmin(9999) {
		t.Error("9999 should not be admin")
	}

	// Empty admin list -> everyone is admin.
	open, err := model.OpenDB(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	b2 := NewBot(newFakeAPI(t), open, nil)
	if !b2.IsAdmin(12345) {
		t.Error("with empty admin list, all users should be admin")
	}
}

func TestHandle_NonCommand(t *testing.T) {
	b, _ := newTestBot(t)
	if b.Handle(msg("just some text", 1001)) {
		t.Error("non-command should not be handled")
	}
}

func TestHandle_UnknownCommand(t *testing.T) {
	b, _ := newTestBot(t)
	if b.Handle(msg("/nonexistent", 1001)) {
		t.Error("unknown command should not be handled")
	}
}

func TestHandle_NonAdmin(t *testing.T) {
	b, _ := newTestBot(t)
	// Non-admin gets a rejection reply; handled=true.
	if !b.Handle(msg("/counters", 9999)) {
		t.Error("non-admin command should be handled (with rejection)")
	}
}

func TestHandle_StartMenu(t *testing.T) {
	b, _ := newTestBot(t)
	if !b.Handle(msg("/start", 1001)) {
		t.Error("/start should be handled")
	}
}

func TestHandle_Counters(t *testing.T) {
	b, db := newTestBot(t)
	// Empty list.
	if !b.Handle(msg("/counters", 1001)) {
		t.Fatal("/counters should be handled")
	}
	if err := db.CreateCounter(context.Background(), &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}); err != nil {
		t.Fatalf("create counter: %v", err)
	}
	if !b.Handle(msg("/counters", 1001)) {
		t.Error("/counters should be handled")
	}
}

func TestHandle_DeleteCounter(t *testing.T) {
	b, db := newTestBot(t)
	if err := db.CreateCounter(context.Background(), &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}); err != nil {
		t.Fatalf("create counter: %v", err)
	}
	// Bad arg.
	if !b.Handle(msg("/deletecounter abc", 1001)) {
		t.Error("bad arg should be handled")
	}
	// Missing counter.
	if !b.Handle(msg("/deletecounter 999", 1001)) {
		t.Error("missing counter should be handled")
	}
	// Real delete.
	if !b.Handle(msg("/deletecounter 1", 1001)) {
		t.Error("delete should be handled")
	}
	if _, err := db.GetCounter(context.Background(), 1); err == nil {
		t.Error("counter should be deleted")
	}
}

func TestHandle_Monitors(t *testing.T) {
	b, db := newTestBot(t)
	if err := db.CreateCounter(context.Background(), &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}); err != nil {
		t.Fatalf("create counter: %v", err)
	}
	// Bad arg.
	if !b.Handle(msg("/monitors", 1001)) {
		t.Error("missing id should be handled")
	}
	// Missing counter.
	if !b.Handle(msg("/monitors 999", 1001)) {
		t.Error("missing counter should be handled")
	}
	// Empty list.
	if !b.Handle(msg("/monitors 1", 1001)) {
		t.Error("empty list should be handled")
	}
	if err := db.CreateMonitor(context.Background(), &model.PageMonitor{CounterID: 1, Name: "Checkout", URLPattern: "/checkout*", Metrics: []string{"visits"}, Enabled: true}); err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	if !b.Handle(msg("/monitors 1", 1001)) {
		t.Error("list should be handled")
	}
}

func TestHandle_DeleteMonitor(t *testing.T) {
	b, db := newTestBot(t)
	if !b.Handle(msg("/deletemonitor abc", 1001)) {
		t.Error("bad arg should be handled")
	}
	if err := db.CreateMonitor(context.Background(), &model.PageMonitor{CounterID: 1, Name: "M", URLPattern: "/x", Metrics: []string{"visits"}, Enabled: true}); err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	if !b.Handle(msg("/deletemonitor 1", 1001)) {
		t.Error("delete should be handled")
	}
	if mons, _ := db.ListMonitors(context.Background(), 1); len(mons) != 0 {
		t.Errorf("monitor should be deleted, got %d", len(mons))
	}
}

func TestHandle_Triggers(t *testing.T) {
	b, db := newTestBot(t)
	if err := db.CreateCounter(context.Background(), &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}); err != nil {
		t.Fatalf("create counter: %v", err)
	}
	if !b.Handle(msg("/triggers", 1001)) {
		t.Error("missing id should be handled")
	}
	if !b.Handle(msg("/triggers 1", 1001)) {
		t.Error("empty list should be handled")
	}
	if err := db.CreateTrigger(context.Background(), &model.Trigger{CounterID: 1, Name: "500s", Condition: "status_code == 500", Threshold: 3, Window: 15, Cooldown: 60, Enabled: true}); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if !b.Handle(msg("/triggers 1", 1001)) {
		t.Error("list should be handled")
	}
}

func TestHandle_DeleteTrigger(t *testing.T) {
	b, db := newTestBot(t)
	if !b.Handle(msg("/deletetrigger abc", 1001)) {
		t.Error("bad arg should be handled")
	}
	if err := db.CreateTrigger(context.Background(), &model.Trigger{CounterID: 1, Name: "t", Condition: "x", Threshold: 1, Window: 30, Cooldown: 60, Enabled: true}); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if !b.Handle(msg("/deletetrigger 1", 1001)) {
		t.Error("delete should be handled")
	}
	if trigs, _ := db.ListTriggers(context.Background(), 1); len(trigs) != 0 {
		t.Errorf("trigger should be deleted, got %d", len(trigs))
	}
}

func TestHandle_Actions(t *testing.T) {
	b, db := newTestBot(t)
	// Empty.
	if !b.Handle(msg("/actions", 1001)) {
		t.Error("empty actions should be handled")
	}
	if err := db.CreateAlertAction(context.Background(), &model.AlertAction{Name: "hook", Type: "webhook", URL: "http://example.com"}); err != nil {
		t.Fatalf("create action: %v", err)
	}
	cid := int64(777)
	if err := db.CreateAlertAction(context.Background(), &model.AlertAction{Name: "tg", Type: "telegram", ChatID: &cid}); err != nil {
		t.Fatalf("create tg action: %v", err)
	}
	if !b.Handle(msg("/actions", 1001)) {
		t.Error("list should be handled")
	}
}

func TestHandle_DeleteAction(t *testing.T) {
	b, db := newTestBot(t)
	if !b.Handle(msg("/deleteaction abc", 1001)) {
		t.Error("bad arg should be handled")
	}
	if err := db.CreateAlertAction(context.Background(), &model.AlertAction{Name: "hook", Type: "webhook", URL: "http://example.com"}); err != nil {
		t.Fatalf("create action: %v", err)
	}
	if !b.Handle(msg("/deleteaction 1", 1001)) {
		t.Error("delete should be handled")
	}
	if actions, _ := db.ListAlertActions(context.Background()); len(actions) != 0 {
		t.Errorf("action should be deleted, got %d", len(actions))
	}
}

func TestHandle_Alerts(t *testing.T) {
	b, db := newTestBot(t)
	// No counter_id arg -> all alerts (empty).
	if !b.Handle(msg("/alerts", 1001)) {
		t.Error("no-arg alerts should be handled")
	}
	// Bad arg.
	if !b.Handle(msg("/alerts abc", 1001)) {
		t.Error("bad arg should be handled")
	}
	// With counter -> empty.
	if err := db.CreateCounter(context.Background(), &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}); err != nil {
		t.Fatalf("create counter: %v", err)
	}
	if !b.Handle(msg("/alerts 1", 1001)) {
		t.Error("empty alerts should be handled")
	}
	// With a real alert.
	trig := &model.Trigger{CounterID: 1, Name: "t", Condition: "x", Threshold: 1, Window: 30, Cooldown: 60, Enabled: true}
	if err := db.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if err := db.CreateAlert(context.Background(), &model.Alert{TriggerID: trig.ID, CounterID: 1, Title: "T", Message: "M", EventCount: 2}); err != nil {
		t.Fatalf("create alert: %v", err)
	}
	if !b.Handle(msg("/alerts 1", 1001)) {
		t.Error("list should be handled")
	}
}

func TestHandle_ReportPoll(t *testing.T) {
	b, _ := newTestBot(t)
	if !b.Handle(msg("/report 1", 1001)) {
		t.Error("/report should be handled")
	}
	if !b.Handle(msg("/poll", 1001)) {
		t.Error("/poll should be handled")
	}
}

// ---- BotActions: multi-step interactive flows ----

// wiredBot returns a Bot wired to its BotActions, mirroring the production
// setup in main.go (NewBot -> NewBotActions -> SetActions).
func wiredBot(t *testing.T) (*Bot, *BotActions, *model.DB) {
	t.Helper()
	b, db := newTestBot(t)
	ba := NewBotActions(b)
	b.SetActions(ba)
	return b, ba, db
}

func TestActions_AddCounter(t *testing.T) {
	b, ba, db := wiredBot(t)
	// Start the flow via the command (records a pending action).
	if !b.Handle(msg("/addcounter", 1001)) {
		t.Fatal("/addcounter should be handled")
	}
	// Respond with the counter data.
	if !ba.Handle(1001, "Shop 12345 tok 60") {
		t.Fatal("add counter should consume the response")
	}
	counters, _ := db.ListCounters(context.Background())
	if len(counters) != 1 {
		t.Fatalf("counters = %d, want 1", len(counters))
	}
	if counters[0].Name != "Shop" || counters[0].CounterID != "12345" || counters[0].PollInterval != 60 {
		t.Errorf("counter = %+v", counters[0])
	}
	// Pending action is cleared after success.
	if ba.Handle(1001, "Shop 12345 tok 60") {
		t.Error("pending action should be cleared after success")
	}

	// Bad format: consumed, no counter created, pending kept.
	ba.recordPending(1001, 42, actionAddCounter)
	if !ba.Handle(1001, "onlytwo") {
		t.Error("bad format should be consumed")
	}
	counters, _ = db.ListCounters(context.Background())
	if len(counters) != 1 {
		t.Errorf("bad format should not create counter, got %d", len(counters))
	}
}

func TestActions_AddMonitor_Inline(t *testing.T) {
	b, ba, db := wiredBot(t)
	if err := db.CreateCounter(context.Background(), &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}); err != nil {
		t.Fatalf("create counter: %v", err)
	}
	// Start the flow via the command.
	if !b.Handle(msg("/addmonitor 1", 1001)) {
		t.Fatal("/addmonitor should be handled")
	}
	// Inline single-message format: name url_pattern [metrics].
	if !ba.Handle(1001, "Checkout /checkout* visits bounces") {
		t.Fatal("inline monitor should be consumed")
	}
	mons, _ := db.ListMonitors(context.Background(), 1)
	if len(mons) != 1 {
		t.Fatalf("monitors = %d, want 1", len(mons))
	}
	if mons[0].Name != "Checkout" || mons[0].URLPattern != "/checkout*" {
		t.Errorf("monitor = %+v", mons[0])
	}
	if len(mons[0].Metrics) != 2 {
		t.Errorf("metrics = %v, want 2", mons[0].Metrics)
	}
}

func TestActions_AddMonitor_StepByStep(t *testing.T) {
	b, ba, db := wiredBot(t)
	if err := db.CreateCounter(context.Background(), &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}); err != nil {
		t.Fatalf("create counter: %v", err)
	}
	if !b.Handle(msg("/addmonitor 1", 1001)) {
		t.Fatal("/addmonitor should be handled")
	}
	// One field -> step-by-step: ask for name.
	if !ba.Handle(1001, "Checkout") {
		t.Fatal("step 0 should be consumed")
	}
	// step 1: name -> ask url.
	if !ba.Handle(1001, "/checkout*") {
		t.Fatal("step 1 should be consumed")
	}
	// step 2: url -> ask metrics.
	if !ba.Handle(1001, "visits") {
		t.Fatal("step 2 should be consumed")
	}
	// step 3: metrics -> save.
	if !ba.Handle(1001, "") {
		t.Fatal("step 3 should be consumed")
	}
	mons, _ := db.ListMonitors(context.Background(), 1)
	if len(mons) != 1 {
		t.Fatalf("monitors = %d, want 1", len(mons))
	}
	// Empty metrics -> default set.
	if len(mons[0].Metrics) != 4 {
		t.Errorf("default metrics = %v, want 4", mons[0].Metrics)
	}
}

func TestActions_AddTrigger(t *testing.T) {
	b, ba, db := wiredBot(t)
	if err := db.CreateCounter(context.Background(), &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}); err != nil {
		t.Fatalf("create counter: %v", err)
	}
	// Bad format (too few parts).
	if !b.Handle(msg("/addtrigger 1", 1001)) {
		t.Fatal("/addtrigger should be handled")
	}
	if !ba.Handle(1001, "name cond") {
		t.Error("bad format should be consumed")
	}
	trigs, _ := db.ListTriggers(context.Background(), 1)
	if len(trigs) != 0 {
		t.Errorf("bad format should not create trigger, got %d", len(trigs))
	}

	// Valid: name condition... threshold window cooldown
	if !b.Handle(msg("/addtrigger 1", 1001)) {
		t.Fatal("/addtrigger should be handled")
	}
	if !ba.Handle(1001, "500s status_code == 500 3 15 60") {
		t.Fatal("valid trigger should be consumed")
	}
	trigs, _ = db.ListTriggers(context.Background(), 1)
	if len(trigs) != 1 {
		t.Fatalf("triggers = %d, want 1", len(trigs))
	}
	if trigs[0].Name != "500s" || trigs[0].Condition != "status_code == 500" {
		t.Errorf("trigger = %+v", trigs[0])
	}
	if trigs[0].Threshold != 3 || trigs[0].Window != 15 || trigs[0].Cooldown != 60 {
		t.Errorf("trigger params = %+v", trigs[0])
	}
}

func TestActions_AddTrigger_MonitorScope(t *testing.T) {
	b, ba, db := wiredBot(t)
	if err := db.CreateCounter(context.Background(), &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}); err != nil {
		t.Fatalf("create counter: %v", err)
	}
	if err := db.CreateMonitor(context.Background(), &model.PageMonitor{CounterID: 1, Name: "M", URLPattern: "/x", Metrics: []string{"visits"}, Enabled: true}); err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	if !b.Handle(msg("/addtrigger 1", 1001)) {
		t.Fatal("/addtrigger should be handled")
	}
	// With monitor_id as the last field.
	if !ba.Handle(1001, "500s status_code == 500 3 15 60 1") {
		t.Fatal("valid trigger should be consumed")
	}
	trigs, _ := db.ListTriggers(context.Background(), 1)
	if len(trigs) != 1 {
		t.Fatalf("triggers = %d, want 1", len(trigs))
	}
	if trigs[0].MonitorID == nil || *trigs[0].MonitorID != 1 {
		t.Errorf("monitor scope = %v, want 1", trigs[0].MonitorID)
	}
}

func TestActions_AddAction(t *testing.T) {
	b, ba, db := wiredBot(t)
	// Bad format.
	if !b.Handle(msg("/addaction", 1001)) {
		t.Fatal("/addaction should be handled")
	}
	if !ba.Handle(1001, "onlyone") {
		t.Error("bad format should be consumed")
	}
	// Unknown type.
	if !b.Handle(msg("/addaction", 1001)) {
		t.Fatal("/addaction should be handled")
	}
	if !ba.Handle(1001, "sms 123") {
		t.Error("unknown type should be consumed")
	}
	actions, _ := db.ListAlertActions(context.Background())
	if len(actions) != 0 {
		t.Errorf("no actions should be created, got %d", len(actions))
	}

	// Telegram action.
	if !b.Handle(msg("/addaction", 1001)) {
		t.Fatal("/addaction should be handled")
	}
	if !ba.Handle(1001, "telegram 777") {
		t.Fatal("telegram action should be consumed")
	}
	// Webhook action.
	if !b.Handle(msg("/addaction", 1001)) {
		t.Fatal("/addaction should be handled")
	}
	if !ba.Handle(1001, "webhook https://example.com/hook") {
		t.Fatal("webhook action should be consumed")
	}
	actions, _ = db.ListAlertActions(context.Background())
	if len(actions) != 2 {
		t.Fatalf("actions = %d, want 2", len(actions))
	}
	var sawTG, sawHook bool
	for _, a := range actions {
		if a.Type == "telegram" && a.ChatID != nil && *a.ChatID == 777 {
			sawTG = true
		}
		if a.Type == "webhook" && a.URL == "https://example.com/hook" {
			sawHook = true
		}
	}
	if !sawTG || !sawHook {
		t.Errorf("expected telegram + webhook actions, got %+v", actions)
	}
}

func TestActions_NoPending(t *testing.T) {
	_, ba, _ := wiredBot(t)
	if ba.Handle(1001, "anything") {
		t.Error("no pending action should not consume")
	}
}

func TestProcessMessage(t *testing.T) {
	_, ba, _ := wiredBot(t)
	// Command -> routed to Bot.Handle.
	if !ba.ProcessMessage(msg("/start", 1001)) {
		t.Error("command should be handled via ProcessMessage")
	}
	// Non-command text with no pending action -> false.
	if ba.ProcessMessage(msg("hello", 1001)) {
		t.Error("text with no pending action should not be consumed")
	}
	// Empty text -> false.
	if ba.ProcessMessage(msg("", 1001)) {
		t.Error("empty text should not be consumed")
	}
}
