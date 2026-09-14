package alert

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/isklv/metrika-alert/internal/model"
)

type fakeTelegram struct {
	mu   sync.Mutex
	sent map[int64]string
	fail bool
}

func (f *fakeTelegram) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return tgbotapi.Message{}, fmt.Errorf("telegram unavailable")
	}
	msg := c.(tgbotapi.MessageConfig)
	if f.sent == nil {
		f.sent = map[int64]string{}
	}
	f.sent[msg.ChatID] = msg.Text
	return tgbotapi.Message{}, nil
}

type fakeVKTeams struct {
	mu   sync.Mutex
	sent map[string]string
	fail bool
}

func (f *fakeVKTeams) SendText(_ context.Context, chatID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return fmt.Errorf("vkteams unavailable")
	}
	if f.sent == nil {
		f.sent = map[string]string{}
	}
	f.sent[chatID] = text
	return nil
}

func testDB(t *testing.T) *model.DB {
	t.Helper()
	db, err := model.OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func addAction(t *testing.T, db *model.DB, a *model.AlertAction) {
	t.Helper()
	if err := db.CreateAlertAction(context.Background(), a); err != nil {
		t.Fatalf("CreateAlertAction(%s): %v", a.Type, err)
	}
}

func TestAlertFansOutToEveryChannel(t *testing.T) {
	db := testDB(t)
	tg, vk := &fakeTelegram{}, &fakeVKTeams{}

	var hookBody []byte
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hookBody, _ = io.ReadAll(r.Body)
	}))
	defer hook.Close()

	chatID := int64(4242)
	addAction(t, db, &model.AlertAction{Name: "tg", Type: "telegram", ChatID: &chatID})
	addAction(t, db, &model.AlertAction{Name: "vk", Type: "vkteams", Target: "team@corp.ru"})
	addAction(t, db, &model.AlertAction{Name: "hook", Type: "webhook", URL: hook.URL})

	r := NewRouter(db, Options{Telegram: tg, VKTeams: vk})
	if err := r.Alert(context.Background(), 1, "🔴 Alert: Магазин", "*Trigger:* Ошибки"); err != nil {
		t.Fatalf("Alert: %v", err)
	}

	if got := tg.sent[4242]; !strings.Contains(got, "Магазин") {
		t.Errorf("telegram got %q", got)
	}
	// VK Teams renders HTML, so the Markdown must have been converted.
	if got := vk.sent["team@corp.ru"]; !strings.Contains(got, "<b>Trigger:</b>") {
		t.Errorf("vkteams got %q, want converted markup", got)
	}
	var payload map[string]any
	if err := json.Unmarshal(hookBody, &payload); err != nil {
		t.Fatalf("webhook payload: %v", err)
	}
	if payload["title"] != "🔴 Alert: Магазин" {
		t.Errorf("webhook title = %v", payload["title"])
	}
}

// Webhooks are independent of the chat bots; they used to be skipped entirely
// whenever no Telegram token was configured.
func TestWebhookDeliveredWithoutAnyBot(t *testing.T) {
	db := testDB(t)

	var called bool
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer hook.Close()

	addAction(t, db, &model.AlertAction{Name: "hook", Type: "webhook", URL: hook.URL})

	r := NewRouter(db, Options{}) // no telegram, no vkteams
	if err := r.Alert(context.Background(), 1, "title", "body"); err != nil {
		t.Fatalf("Alert: %v", err)
	}
	if !called {
		t.Fatal("webhook was not called")
	}
}

// With no destinations configured, alerts must still reach the admins from
// config instead of being logged and dropped.
func TestAlertFallsBackToConfiguredAdmins(t *testing.T) {
	db := testDB(t)
	tg, vk := &fakeTelegram{}, &fakeVKTeams{}

	r := NewRouter(db, Options{
		Telegram: tg, TelegramAdmins: []int64{11, 22},
		VKTeams: vk, VKTeamsAdmins: []string{"boss@corp.ru"},
	})
	if err := r.Alert(context.Background(), 1, "title", "body"); err != nil {
		t.Fatalf("Alert: %v", err)
	}

	if len(tg.sent) != 2 {
		t.Errorf("telegram admins reached: %d, want 2", len(tg.sent))
	}
	if _, ok := vk.sent["boss@corp.ru"]; !ok {
		t.Error("vkteams admin was not reached")
	}
}

// One broken destination must not stop the others.
func TestAlertContinuesPastAFailingDestination(t *testing.T) {
	db := testDB(t)
	vk := &fakeVKTeams{}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer dead.Close()

	addAction(t, db, &model.AlertAction{Name: "hook", Type: "webhook", URL: dead.URL})
	addAction(t, db, &model.AlertAction{Name: "vk", Type: "vkteams", Target: "team@corp.ru"})

	r := NewRouter(db, Options{VKTeams: vk})
	if err := r.Alert(context.Background(), 1, "title", "body"); err != nil {
		t.Fatalf("Alert: %v", err)
	}
	if _, ok := vk.sent["team@corp.ru"]; !ok {
		t.Fatal("delivery stopped at the failing webhook")
	}
}

// Silent failure is the worst outcome for an alerting service: if nothing was
// delivered, the caller has to find out.
func TestAlertReportsTotalDeliveryFailure(t *testing.T) {
	db := testDB(t)
	vk := &fakeVKTeams{fail: true}
	addAction(t, db, &model.AlertAction{Name: "vk", Type: "vkteams", Target: "team@corp.ru"})

	r := NewRouter(db, Options{VKTeams: vk})
	if err := r.Alert(context.Background(), 1, "title", "body"); err == nil {
		t.Fatal("expected an error when every destination failed")
	}
}

// A vkteams destination with no bot configured must be reported, not ignored.
func TestVKTeamsActionWithoutClientIsAnError(t *testing.T) {
	db := testDB(t)
	addAction(t, db, &model.AlertAction{Name: "vk", Type: "vkteams", Target: "team@corp.ru"})

	r := NewRouter(db, Options{})
	if err := r.Alert(context.Background(), 1, "title", "body"); err == nil {
		t.Fatal("expected an error for a vkteams action with no bot token")
	}
}

func TestTelegramRetriesAsPlainTextOnMarkupFailure(t *testing.T) {
	db := testDB(t)
	// Rejects the Markdown attempt, accepts the plain-text retry.
	tg := &rejectingTelegram{}

	chatID := int64(7)
	addAction(t, db, &model.AlertAction{Name: "tg", Type: "telegram", ChatID: &chatID})

	r := NewRouter(db, Options{Telegram: tg})
	if err := r.Alert(context.Background(), 1, "Отчёт", "*Посещения:* 100 (не *закрытая* разметка"); err != nil {
		t.Fatalf("Alert: %v", err)
	}
	if tg.attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (markdown then plain)", tg.attempts)
	}
	if strings.Contains(tg.lastText, "*") {
		t.Errorf("retry still carried markup: %q", tg.lastText)
	}
}

type rejectingTelegram struct {
	attempts int
	lastText string
}

func (f *rejectingTelegram) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	f.attempts++
	msg := c.(tgbotapi.MessageConfig)
	f.lastText = msg.Text
	if msg.ParseMode == tgbotapi.ModeMarkdown {
		return tgbotapi.Message{}, fmt.Errorf("can't parse entities")
	}
	return tgbotapi.Message{}, nil
}

func TestChannelsReportsConfiguredTransports(t *testing.T) {
	db := testDB(t)

	if got := NewRouter(db, Options{}).Channels(); got[0] != "none (webhooks only)" {
		t.Errorf("Channels() = %v", got)
	}
	got := NewRouter(db, Options{Telegram: &fakeTelegram{}, VKTeams: &fakeVKTeams{}}).Channels()
	if strings.Join(got, ",") != "telegram,vkteams" {
		t.Errorf("Channels() = %v", got)
	}
}

// Trigger conditions are Metrika field names. Stripping underscores along with
// the display markup turned "status_code == 500" into "statuscode == 500" —
// the alert then misreported the very rule that fired it.
func TestFieldNamesSurviveMarkupStripping(t *testing.T) {
	const message = "*Condition:* `status_code == 500`\n*Page:* `page_url contains /checkout`\n*Order:* `order_id`"

	got := stripMarkdown(message)

	for _, field := range []string{"status_code", "page_url", "order_id"} {
		if !strings.Contains(got, field) {
			t.Errorf("stripMarkdown corrupted %q:\n%s", field, got)
		}
	}
	if strings.ContainsAny(got, "*`") {
		t.Errorf("display markers survived: %q", got)
	}
}

func TestWebhookPayloadKeepsFieldNames(t *testing.T) {
	db := testDB(t)

	var body []byte
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
	}))
	defer hook.Close()

	addAction(t, db, &model.AlertAction{Name: "hook", Type: "webhook", URL: hook.URL})

	r := NewRouter(db, Options{})
	if err := r.Alert(context.Background(), 1, "Alert", "*Condition:* `status_code == 500`"); err != nil {
		t.Fatalf("Alert: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if msg, _ := payload["message"].(string); !strings.Contains(msg, "status_code == 500") {
		t.Errorf("webhook message = %q", msg)
	}
}
