package alert

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/isklv/metrika-alert/internal/model"
)

func openAlertDB(t *testing.T) *model.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := model.OpenDB(filepath.Join(dir, "alert.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestNewRouter_EmptyToken(t *testing.T) {
	db := openAlertDB(t)
	r, err := NewRouter("", db)
	if err != nil {
		t.Fatalf("NewRouter(\"\"): %v", err)
	}
	if r == nil || r.bot != nil {
		t.Errorf("expected non-nil router with nil bot, got %+v", r)
	}
	if r.chatIDs == nil {
		t.Error("chatIDs should be initialized")
	}
}

func TestAlert_NoBotSkips(t *testing.T) {
	db := openAlertDB(t)
	r, _ := NewRouter("", db)
	// With no bot, Alert short-circuits and returns nil even with actions.
	if err := r.Alert(context.Background(), 1, "title", "msg"); err != nil {
		t.Errorf("Alert (no bot) = %v, want nil", err)
	}
}

func TestSendWebhook_Success(t *testing.T) {
	var gotBody map[string]any
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	r := &Router{webhook: srv.Client(), chatIDs: map[string]int64{}}
	if err := r.sendWebhook(srv.URL, 42, "Title", "bold *msg*"); err != nil {
		t.Fatalf("sendWebhook: %v", err)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotBody["counter_id"] != float64(42) {
		t.Errorf("counter_id = %v", gotBody["counter_id"])
	}
	if gotBody["title"] != "Title" {
		t.Errorf("title = %v", gotBody["title"])
	}
	// Asterisks stripped from message.
	if msg, _ := gotBody["message"].(string); msg != "bold msg" {
		t.Errorf("message = %q, want asterisks stripped", msg)
	}
	if gotBody["timestamp"] == "" {
		t.Error("timestamp should be set")
	}
}

func TestSendWebhook_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	r := &Router{webhook: srv.Client(), chatIDs: map[string]int64{}}
	err := r.sendWebhook(srv.URL, 1, "t", "m")
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("sendWebhook = %v, want 400 error", err)
	}
}

func TestSendWebhook_ConnectionError(t *testing.T) {
	// Point at a closed server to force a connection error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	r := &Router{webhook: &http.Client{}, chatIDs: map[string]int64{}}
	if err := r.sendWebhook(url, 1, "t", "m"); err == nil {
		t.Error("expected connection error, got nil")
	}
}

func TestChatCache(t *testing.T) {
	r := &Router{chatIDs: map[string]int64{}}

	if got := r.ChatIDFor("alice"); got != 0 {
		t.Errorf("ChatIDFor unknown = %d, want 0", got)
	}

	r.AddChatID("alice", 111)
	if got := r.ChatIDFor("alice"); got != 111 {
		t.Errorf("ChatIDFor alice = %d, want 111", got)
	}

	// Overwrite.
	r.AddChatID("alice", 222)
	if got := r.ChatIDFor("alice"); got != 222 {
		t.Errorf("ChatIDFor alice after overwrite = %d, want 222", got)
	}

	// RecordDeliveryChat uses a "chat-<id>" key.
	r.RecordDeliveryChat(999)
	if got := r.ChatIDFor("chat-999"); got != 999 {
		t.Errorf("ChatIDFor chat-999 = %d, want 999", got)
	}
}

func TestRecordDeliveryChat_NilMap(t *testing.T) {
	r := &Router{} // chatIDs nil
	r.RecordDeliveryChat(5)
	if got := r.ChatIDFor("chat-5"); got != 5 {
		t.Errorf("ChatIDFor chat-5 = %d, want 5", got)
	}
}

func TestNotifyAdmin_NoBot(t *testing.T) {
	r := &Router{chatIDs: map[string]int64{}}
	if err := r.NotifyAdmin("hi"); err == nil {
		t.Error("NotifyAdmin (no bot) should error")
	}
}

// fakeTGAPI returns a *tgbotapi.BotAPI pointed at an httptest server that
// answers getMe with a User and everything else with a Message. The handler
// records each request's form body so tests can assert on what was sent.
func fakeTGAPI(t *testing.T, logRequests *[]string) *tgbotapi.BotAPI {
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
		// Log non-getMe calls (e.g. sendMessage) for assertions.
		_ = r.ParseForm()
		if logRequests != nil {
			*logRequests = append(*logRequests, r.URL.Path+"|"+r.PostForm.Get("text"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 1, "chat": map[string]any{"id": 1}},
		})
	}))
	t.Cleanup(ts.Close)
	// MakeRequest builds the URL via fmt.Sprintf(endpoint, token, method).
	api, err := tgbotapi.NewBotAPIWithClient("test-token", ts.URL+"/%s/%s", ts.Client())
	if err != nil {
		t.Fatalf("new bot api: %v", err)
	}
	return api
}

func TestNewRouter_WithBot(t *testing.T) {
	db := openAlertDB(t)
	api := fakeTGAPI(t, nil)
	r := &Router{bot: api, db: db, webhook: &http.Client{}, chatIDs: map[string]int64{}}
	if r == nil {
		t.Fatal("router should be non-nil")
	}
}

func TestAlert_TelegramAction(t *testing.T) {
	db := openAlertDB(t)
	var sent []string
	api := fakeTGAPI(t, &sent)
	r := &Router{bot: api, db: db, webhook: &http.Client{}, chatIDs: map[string]int64{}}

	cid := int64(777)
	if err := db.CreateAlertAction(context.Background(), &model.AlertAction{Name: "tg", Type: "telegram", ChatID: &cid}); err != nil {
		t.Fatalf("create action: %v", err)
	}

	if err := r.Alert(context.Background(), 1, "Title", "Body"); err != nil {
		t.Fatalf("Alert: %v", err)
	}
	if len(sent) != 1 {
		t.Fatalf("sent = %d messages, want 1", len(sent))
	}
	if !strings.Contains(sent[0], "TitleBody") {
		t.Errorf("message = %q, want title+body", sent[0])
	}
}

func TestAlert_WebhookAction(t *testing.T) {
	db := openAlertDB(t)
	var gotBody map[string]any
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ws.Close)

	r := &Router{bot: fakeTGAPI(t, nil), db: db, webhook: ws.Client(), chatIDs: map[string]int64{}}
	if err := db.CreateAlertAction(context.Background(), &model.AlertAction{Name: "wh", Type: "webhook", URL: ws.URL}); err != nil {
		t.Fatalf("create action: %v", err)
	}

	if err := r.Alert(context.Background(), 5, "T", "M"); err != nil {
		t.Fatalf("Alert: %v", err)
	}
	if gotBody["title"] != "T" || gotBody["counter_id"] != float64(5) {
		t.Errorf("webhook payload = %v", gotBody)
	}
}

func TestAlert_NoActions_Broadcasts(t *testing.T) {
	db := openAlertDB(t)
	var sent []string
	r := &Router{bot: fakeTGAPI(t, &sent), db: db, webhook: &http.Client{}, chatIDs: map[string]int64{}}
	// No actions configured -> broadcastToAdmins path (log-only, no error).
	if err := r.Alert(context.Background(), 1, "Title", "Body"); err != nil {
		t.Fatalf("Alert (no actions): %v", err)
	}
	// broadcastToAdmins does not send messages (log-only fallback).
	if len(sent) != 0 {
		t.Errorf("sent = %d, want 0 (broadcast is log-only)", len(sent))
	}
}

func TestNotifyAdmin_WithBot(t *testing.T) {
	var sent []string
	r := &Router{bot: fakeTGAPI(t, &sent), db: nil, webhook: &http.Client{}, chatIDs: map[string]int64{}}
	r.AddChatID("admin", 42)
	if err := r.NotifyAdmin("hello"); err != nil {
		t.Fatalf("NotifyAdmin: %v", err)
	}
	if len(sent) != 1 || !strings.Contains(sent[0], "hello") {
		t.Errorf("sent = %v, want 1 message containing 'hello'", sent)
	}
}

func TestSendToChat_Truncation(t *testing.T) {
	var sent []string
	r := &Router{bot: fakeTGAPI(t, &sent), db: nil, webhook: &http.Client{}, chatIDs: map[string]int64{}}
	long := strings.Repeat("x", 4000)
	if err := r.sendToChat(1, long); err != nil {
		t.Fatalf("sendToChat: %v", err)
	}
	if len(sent) != 1 {
		t.Fatalf("sent = %d, want 1", len(sent))
	}
	// sent[0] is "path|text"; measure the text portion only.
	text := sent[0]
	if i := strings.Index(text, "|"); i != -1 {
		text = text[i+1:]
	}
	if !strings.Contains(text, "сообщение обрезано") {
		t.Error("long message should be truncated with a marker")
	}
	if len(text) > 3800 {
		t.Errorf("truncated length = %d, want <= 3800", len(text))
	}
}
