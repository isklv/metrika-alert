package vkteams

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient points a client at a stub server.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := New("test-token", Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestSendTextPassesChatAndMarkup(t *testing.T) {
	var got *http.Request
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Write([]byte(`{"ok":true,"msgId":"123"}`))
	})

	if err := c.SendText(context.Background(), "user@corp.ru", "<b>Alert</b>"); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	q := got.URL.Query()
	if q.Get("chatId") != "user@corp.ru" {
		t.Errorf("chatId = %q, want user@corp.ru", q.Get("chatId"))
	}
	if q.Get("text") != "<b>Alert</b>" {
		t.Errorf("text = %q", q.Get("text"))
	}
	if q.Get("parseMode") != "HTML" {
		t.Errorf("parseMode = %q, want HTML", q.Get("parseMode"))
	}
	if q.Get("token") != "test-token" {
		t.Error("token was not sent")
	}
}

// The API answers HTTP 200 with ok:false for an invalid token or unknown chat.
// Treating that as success would silently drop every alert.
func TestCallSurfacesAPIRejection(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":false,"description":"Invalid chatId"}`))
	})

	err := c.SendText(context.Background(), "nobody@corp.ru", "hi")
	if err == nil {
		t.Fatal("expected an error for ok:false, got nil")
	}
	if !strings.Contains(err.Error(), "Invalid chatId") {
		t.Errorf("error does not carry the API description: %v", err)
	}
}

func TestSendTextRejectsEmptyChat(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("request should not have been sent")
	})
	if err := c.SendText(context.Background(), "", "hi"); err == nil {
		t.Fatal("expected an error for an empty chatId")
	}
}

func TestGetEventsReturnsMessages(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("lastEventId"); got != "7" {
			t.Errorf("lastEventId = %q, want 7", got)
		}
		w.Write([]byte(`{"ok":true,"events":[
			{"eventId":8,"type":"newMessage","payload":{
				"msgId":"m1","text":"/counters",
				"chat":{"chatId":"c1","type":"private"},
				"from":{"userId":"admin@corp.ru","firstName":"Admin"}}}]}`))
	})

	events, err := c.GetEvents(context.Background(), 7, time.Second)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}

	payload, err := ParseNewMessage(events[0])
	if err != nil {
		t.Fatalf("ParseNewMessage: %v", err)
	}
	if payload.Text != "/counters" {
		t.Errorf("text = %q", payload.Text)
	}
	if payload.From.UserID != "admin@corp.ru" {
		t.Errorf("userId = %q", payload.From.UserID)
	}
	if payload.Chat.ChatID != "c1" {
		t.Errorf("chatId = %q", payload.Chat.ChatID)
	}
}

func TestParseNewMessageRejectsOtherEventTypes(t *testing.T) {
	_, err := ParseNewMessage(Event{EventID: 1, Type: "editedMessage"})
	if err == nil {
		t.Fatal("expected an error for a non-newMessage event")
	}
}

func TestNewRequiresToken(t *testing.T) {
	if _, err := New("", Options{}); err == nil {
		t.Fatal("expected an error for an empty token")
	}
}

func TestNewDefaultsToCloudEndpoint(t *testing.T) {
	c, err := New("t", Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.BaseURL() != DefaultBaseURL {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL(), DefaultBaseURL)
	}
}

func TestNewKeepsOnPremiseEndpoint(t *testing.T) {
	const onPrem = "https://myteam.corp.example/bot/v1"
	c, err := New("t", Options{BaseURL: onPrem + "/"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.BaseURL() != onPrem {
		t.Errorf("BaseURL = %q, want %q (trailing slash trimmed)", c.BaseURL(), onPrem)
	}
}
