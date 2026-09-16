package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/isklv/metrika-alert/internal/model"
)

type fakeDocTransport struct {
	fakeTransport
	mu       sync.Mutex
	sentDocs []struct {
		filename string
		content  []byte
		caption  string
	}
}

func (f *fakeDocTransport) SendDocument(_ context.Context, chatID, filename string, content []byte, caption string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sentDocs = append(f.sentDocs, struct {
		filename string
		content  []byte
		caption  string
	}{filename: filename, content: content, caption: caption})
	return nil
}

func (f *fakeDocTransport) lastDoc() (string, []byte, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sentDocs) == 0 {
		return "", nil, ""
	}
	d := f.sentDocs[len(f.sentDocs)-1]
	return d.filename, d.content, d.caption
}

func newTestDocBot(t *testing.T, admins ...string) (*Bot, *fakeDocTransport, *model.DB) {
	t.Helper()
	db, err := model.OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	tr := &fakeDocTransport{}
	return New(tr, db, nil, &fakeMetrika{}, admins), tr, db
}

func TestExportCommand(t *testing.T) {
	b, tr, db := newTestDocBot(t, admin)
	ctx := context.Background()

	// Seed database
	c := &model.Counter{
		Name:         "Online Store",
		CounterID:    "12345678",
		OAuthToken:   "secret_oauth_token",
		PollInterval: 30,
	}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}

	trig := &model.Trigger{
		CounterID:     c.ID,
		Name:          "Checkout drop",
		Metric:        "visits",
		Direction:     "drop",
		DeviationPct:  40,
		MinBaseline:   10,
		BaselineWeeks: 4,
		URLFilter:     "/checkout",
		URLMatch:      "contains",
		Cooldown:      180,
		Enabled:       true,
	}
	if err := db.CreateTrigger(ctx, trig); err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}

	if err := db.SetSetting(ctx, model.SettingReportSchedule, "10:00"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	// 1. Export as document (default behavior with DocumentTransport)
	say(t, b, "/export")
	filename, content, caption := tr.lastDoc()
	if filename != "metrika-alert-settings.json" {
		t.Errorf("expected filename metrika-alert-settings.json, got %q", filename)
	}
	if !strings.Contains(caption, "Online Store") && !strings.Contains(caption, "Счётчики: 1") {
		t.Errorf("unexpected caption: %q", caption)
	}

	var parsed model.ExportData
	if err := json.Unmarshal(content, &parsed); err != nil {
		t.Fatalf("exported document is not valid JSON: %v", err)
	}
	if len(parsed.Counters) != 1 || parsed.Counters[0].OAuthToken != "secret_oauth_token" {
		t.Errorf("exported data mismatch: %+v", parsed)
	}

	// 2. Export text mode
	say(t, b, "/export text")
	lastMsg := tr.last()
	if !strings.Contains(lastMsg, "Online Store") || !strings.Contains(lastMsg, "secret_oauth_token") {
		t.Errorf("expected text export with details, got: %s", lastMsg)
	}

	// 3. Export safe (no token)
	say(t, b, "/export safe text")
	lastMsgSafe := tr.last()
	if strings.Contains(lastMsgSafe, "secret_oauth_token") {
		t.Errorf("safe export must not leak oauth token: %s", lastMsgSafe)
	}

	// 4. Export specific counter
	say(t, b, fmt.Sprintf("/export %d text", c.ID))
	lastMsgSpecific := tr.last()
	if !strings.Contains(lastMsgSpecific, "Checkout drop") {
		t.Errorf("counter export should contain trigger, got: %s", lastMsgSpecific)
	}

	// 5. Export unknown counter
	say(t, b, "/export 99999 text")
	if !strings.Contains(tr.last(), "не найден") {
		t.Errorf("expected not found error, got: %s", tr.last())
	}
}

func TestImportCommandInteractive(t *testing.T) {
	b, tr, db := newTestBot(t, admin)
	ctx := context.Background()

	// 1. Initiate /import
	say(t, b, "/import")
	if !strings.Contains(tr.last(), "Пришли файл настроек") {
		t.Fatalf("expected prompt for import, got: %s", tr.last())
	}

	// 2. Send JSON payload
	rawJSON := `{
  "version": 1,
  "schedule": "14:00",
  "counters": [
    {
      "name": "Imported Counter",
      "counter_id": "77889900",
      "oauth_token": "imported_token",
      "poll_interval_minutes": 20,
      "triggers": [
        {
          "name": "Spike Alert",
          "metric": "visits",
          "direction": "rise",
          "deviation_percent": 50
        }
      ]
    }
  ]
}`
	say(t, b, rawJSON)
	lastReply := tr.last()
	if !strings.Contains(lastReply, "Импорт настроек завершён") || !strings.Contains(lastReply, "добавлено 1") {
		t.Fatalf("expected success import message, got: %s", lastReply)
	}

	// Verify DB state
	counters, err := db.ListCounters(ctx)
	if err != nil || len(counters) != 1 {
		t.Fatalf("expected 1 counter, got %d (err: %v)", len(counters), err)
	}
	if counters[0].Name != "Imported Counter" || counters[0].CounterID != "77889900" {
		t.Errorf("imported counter mismatch: %+v", counters[0])
	}

	triggers, err := db.ListTriggers(ctx, counters[0].ID)
	if err != nil || len(triggers) != 1 {
		t.Fatalf("expected 1 trigger, got %d (err: %v)", len(triggers), err)
	}
	if triggers[0].Name != "Spike Alert" || triggers[0].Direction != "rise" {
		t.Errorf("imported trigger mismatch: %+v", triggers[0])
	}
}

func TestImportCommandWithDirectJSON(t *testing.T) {
	b, tr, db := newTestBot(t, admin)
	ctx := context.Background()

	raw := `/import {"version": 1, "counters": [{"name": "Direct", "counter_id": "111222", "poll_interval_minutes": 10}]}`
	say(t, b, raw)

	if !strings.Contains(tr.last(), "Импорт настроек завершён") {
		t.Fatalf("expected immediate import success, got: %s", tr.last())
	}

	counters, err := db.ListCounters(ctx)
	if err != nil || len(counters) != 1 || counters[0].CounterID != "111222" {
		t.Fatalf("expected imported counter, got: %+v (err: %v)", counters, err)
	}
}

func TestImportCommandWithDocumentData(t *testing.T) {
	b, tr, db := newTestBot(t, admin)
	ctx := context.Background()

	docJSON := []byte(`{
  "version": 1,
  "counters": [
    {
      "name": "DocCounter",
      "counter_id": "555666",
      "oauth_token": "token555"
    }
  ]
}`)

	// User sends file with caption /import
	b.Handle(ctx, Message{
		UserID: admin,
		ChatID: "chat1",
		Text:   "/import",
		Data:   docJSON,
	})

	if !strings.Contains(tr.last(), "Импорт настроек завершён") {
		t.Fatalf("expected import success from document upload, got: %s", tr.last())
	}

	counters, err := db.ListCounters(ctx)
	if err != nil || len(counters) != 1 || counters[0].Name != "DocCounter" {
		t.Fatalf("expected counter from document, got: %+v", counters)
	}
}

func TestImportReplaceMode(t *testing.T) {
	b, tr, db := newTestBot(t, admin)
	ctx := context.Background()

	// Initial counter
	c := &model.Counter{Name: "Old Counter", CounterID: "1111", OAuthToken: "tok"}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}

	replaceJSON := `/import replace {
  "version": 1,
  "counters": [
    {"name": "New Counter", "counter_id": "2222", "oauth_token": "tok2"}
  ]
}`
	say(t, b, replaceJSON)

	if !strings.Contains(tr.last(), "полная замена / replace") {
		t.Fatalf("expected replace mode notice, got: %s", tr.last())
	}

	counters, err := db.ListCounters(ctx)
	if err != nil || len(counters) != 1 || counters[0].CounterID != "2222" {
		t.Fatalf("expected only new counter after replace, got: %+v", counters)
	}
}

func TestImportValidationFailure(t *testing.T) {
	b, tr, _ := newTestBot(t, admin)

	say(t, b, `/import {"counters": [{"name": "Bad", "counter_id": "not-a-number"}]}`)
	if !strings.Contains(tr.last(), "Ошибка импорта") || !strings.Contains(tr.last(), "должен быть числом") {
		t.Fatalf("expected validation error, got: %s", tr.last())
	}
}

func TestFileUploadedWithoutPendingAction(t *testing.T) {
	b, tr, _ := newTestBot(t, admin)
	ctx := context.Background()

	b.Handle(ctx, Message{
		UserID: admin,
		ChatID: "chat1",
		Text:   "{}",
		Data:   []byte("{}"),
	})

	if !strings.Contains(tr.last(), "Получен файл") {
		t.Fatalf("expected guidance when file is received without flow, got: %s", tr.last())
	}
}
