package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/isklv/metrika-alert/internal/alert"
	"github.com/isklv/metrika-alert/internal/bot"
	"github.com/isklv/metrika-alert/internal/model"
)

// ---- helpers ----

// newFakeTG returns a *tgbotapi.BotAPI that talks to an httptest server
// answering every request with a successful Telegram API envelope.
func newFakeTG(t *testing.T) *tgbotapi.BotAPI {
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
	api, err := tgbotapi.NewBotAPIWithClient("test-token", ts.URL+"/%s/%s", ts.Client())
	if err != nil {
		t.Fatalf("new bot api: %v", err)
	}
	return api
}

func cmdMsg(text string, from int64) *tgbotapi.Message {
	m := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 42},
		From: &tgbotapi.User{ID: from},
		Text: text,
	}
	length := len(text)
	if i := strings.Index(text, " "); i != -1 {
		length = i
	}
	m.Entities = []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: length}}
	return m
}

func plainMsg(text string, from int64) *tgbotapi.Message {
	return &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 42},
		From: &tgbotapi.User{ID: from},
		Text: text,
	}
}

// sendSig sends a signal to the test process itself. The signalContext
// goroutine under test has registered a handler for it via signal.Notify.
func sendSig(t *testing.T, sig syscall.Signal) {
	t.Helper()
	// Give the signalContext goroutine time to register its handler; a
	// signal delivered before that would kill the test process.
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), sig); err != nil {
		t.Fatalf("send %s: %v", sig, err)
	}
}

// ---- signalContext ----

func TestSignalContext_Cancel(t *testing.T) {
	ctx, cancel := signalContext()
	defer cancel()
	if err := ctx.Err(); err != nil {
		t.Fatalf("ctx should not be done yet: %v", err)
	}
	cancel()
	if ctx.Err() != context.Canceled {
		t.Fatalf("ctx.Err() = %v, want context.Canceled", ctx.Err())
	}
}

func TestSignalContext_Signal(t *testing.T) {
	ctx, cancel := signalContext()
	defer cancel()

	sendSig(t, syscall.SIGINT)

	select {
	case <-ctx.Done():
		if ctx.Err() != context.Canceled {
			t.Fatalf("ctx.Err() = %v, want context.Canceled", ctx.Err())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ctx was not canceled after SIGINT")
	}
}

// ---- runReportsLoop ----

type fakeRunner struct {
	calls atomic.Int64
	err   error
}

func (f *fakeRunner) RunReports(ctx context.Context) error {
	f.calls.Add(1)
	return f.err
}

func TestRunReportsLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &fakeRunner{}
	done := make(chan struct{})
	go func() {
		runReportsLoop(ctx, r, 10*time.Millisecond)
		close(done)
	}()

	time.Sleep(80 * time.Millisecond)
	if got := r.calls.Load(); got < 2 {
		t.Errorf("RunReports calls = %d, want >= 2", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runReportsLoop did not exit on ctx cancel")
	}
}

func TestRunReportsLoop_ErrorDoesNotStopLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &fakeRunner{err: errors.New("metrika down")}
	done := make(chan struct{})
	go func() {
		runReportsLoop(ctx, r, 10*time.Millisecond)
		close(done)
	}()

	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runReportsLoop did not exit on ctx cancel")
	}
	if r.calls.Load() == 0 {
		t.Error("RunReports should keep being called despite errors")
	}
}

// ---- handleUpdates ----

func TestHandleUpdates(t *testing.T) {
	db, err := model.OpenDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	api := newFakeTG(t)
	b := bot.NewBot(api, db, []int64{1001})
	ba := bot.NewBotActions(b)
	b.SetActions(ba)

	ch := make(chan tgbotapi.Update, 4)
	done := make(chan struct{})
	go func() {
		handleUpdates(ch, ba)
		close(done)
	}()

	// Command message -> routed to ProcessMessage -> Bot.Handle (menu sent).
	ch <- tgbotapi.Update{UpdateID: 1, Message: cmdMsg("/start", 1001)}
	// Plain text -> ProcessMessage (no pending action, not consumed).
	ch <- tgbotapi.Update{UpdateID: 2, Message: plainMsg("hello", 1001)}
	// Empty text -> skipped by the filter.
	ch <- tgbotapi.Update{UpdateID: 3, Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 42}}}
	// Nil message (e.g. edited message) -> skipped by the filter.
	ch <- tgbotapi.Update{UpdateID: 4}

	close(ch)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleUpdates did not exit when the channel closed")
	}
}

// ---- main ----

// withMainArgs runs main() in a goroutine with the given config path and
// returns a channel closed when main returns.
func runMain(t *testing.T, cfgPath string) <-chan struct{} {
	t.Helper()
	oldArgs := os.Args
	os.Args = []string{"metrika-alert", cfgPath}
	t.Cleanup(func() { os.Args = oldArgs })

	done := make(chan struct{})
	go func() {
		main()
		close(done)
	}()
	return done
}

func writeConfig(t *testing.T, dir, yaml string) string {
	t.Helper()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestMain_NoBot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("METRIKA_BOT_TOKEN", "")
	t.Setenv("METRIKA_PROXY_URL", "")

	addr := freeAddr(t)
	t.Setenv("METRIKA_API_ENABLED", "true")
	t.Setenv("METRIKA_API_LISTEN", addr)

	cfgPath := writeConfig(t, dir, "report_interval_hours: 1\n")
	done := runMain(t, cfgPath)

	// Wait for the REST API to come up.
	deadline := time.Now().Add(5 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/api/status")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.Contains(string(body), "running") {
				ready = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("API server did not become ready")
	}

	// Shut down via SIGTERM; main should exit promptly.
	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("main did not exit after SIGTERM")
	}
}

func TestMain_WithBot(t *testing.T) {
	// Fake Telegram API: getMe, long-polled getUpdates (first call delivers
	// a /start command), and sendMessage (menu reply).
	var sentUpdate atomic.Bool
	var menuSent atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/getMe"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"result": map[string]any{"id": 1, "is_bot": true, "first_name": "Test", "username": "test_bot"},
			})
		case strings.Contains(r.URL.Path, "/getUpdates"):
			// Simulate long polling so the library does not busy-loop.
			time.Sleep(50 * time.Millisecond)
			if !sentUpdate.Swap(true) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok": true,
					"result": []map[string]any{{
						"update_id": 10,
						"message": map[string]any{
							"message_id": 1,
							"chat":       map[string]any{"id": 42, "type": "private"},
							"from":       map[string]any{"id": 1001, "first_name": "U"},
							"text":       "/start",
							"entities":   []map[string]any{{"type": "bot_command", "offset": 0, "length": 6}},
						},
					}},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []any{}})
		case strings.Contains(r.URL.Path, "/sendMessage"):
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "Metrika") {
				menuSent.Store(true)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"result": map[string]any{"message_id": 1, "chat": map[string]any{"id": 42}},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"result": map[string]any{"message_id": 1, "chat": map[string]any{"id": 42}},
			})
		}
	}))
	defer ts.Close()

	// Inject fakes: the bot talks to the fake endpoint (ignoring the proxy
	// client built by main, which is exercised separately), and the alert
	// router is created without a bot so no real getMe happens.
	oldNewBotAPI := newBotAPI
	newBotAPI = func(token string, client *http.Client) (*tgbotapi.BotAPI, error) {
		return tgbotapi.NewBotAPIWithClient(token, ts.URL+"/%s/%s", ts.Client())
	}
	t.Cleanup(func() { newBotAPI = oldNewBotAPI })

	oldNewRouter := newAlertRouter
	newAlertRouter = func(token string, db *model.DB) (*alert.Router, error) {
		return alert.NewRouter("", db)
	}
	t.Cleanup(func() { newAlertRouter = oldNewRouter })

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("METRIKA_API_ENABLED", "")

	// proxy_url exercises the proxy branch in main; the injected newBotAPI
	// ignores the resulting client, so requests still hit the fake server.
	cfgPath := writeConfig(t, dir,
		"telegram:\n  bot_token: fake-token\n  admin_ids: [1001]\n  proxy_url: http://127.0.0.1:1\nreport_interval_hours: 1\n")
	done := runMain(t, cfgPath)

	// Wait until the /start update has been fetched and the menu sent —
	// proves the full path: GetUpdatesChan -> handleUpdates -> ProcessMessage
	// -> Bot.Handle -> sendMenu -> api.Send.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if menuSent.Load() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !menuSent.Load() {
		t.Fatal("bot did not process the /start update and send the menu")
	}

	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("main did not exit after SIGTERM")
	}
}
