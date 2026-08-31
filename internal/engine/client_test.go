package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

func TestSplitComma(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"123,456", []string{"123", "456"}},
		{"123", []string{"123"}},
		{" 123 , 456 ", []string{"123", "456"}},
		{"123,,456", []string{"123", "456"}},
		{"", nil},
		{",,", nil},
	}
	for _, tt := range tests {
		got := splitComma(tt.in)
		if len(got) != len(tt.want) {
			t.Errorf("splitComma(%q) = %v, want %v", tt.in, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitComma(%q) = %v, want %v", tt.in, got, tt.want)
				break
			}
		}
	}
}

func TestParseEvent(t *testing.T) {
	hit := Hit{
		"event_time":  "2026-08-01T10:20:30Z",
		"page_url":    "/checkout/step1",
		"title":       "Checkout",
		"status_code": float64(500),
		"revenue":     float64(1200.5),
		"order_id":    "ORD-1",
		"client_id":   "c-1",
		"user_id":     "u-1",
		"goals_id":    "42, 99",
	}
	ev := ParseEvent(hit)

	wantTime := time.Date(2026, 8, 1, 10, 20, 30, 0, time.UTC)
	if !ev.EventTime.Equal(wantTime) {
		t.Errorf("EventTime = %v, want %v", ev.EventTime, wantTime)
	}
	if ev.PageURL != "/checkout/step1" || ev.Title != "Checkout" {
		t.Errorf("PageURL/Title = %q/%q", ev.PageURL, ev.Title)
	}
	if ev.Status != "500" {
		t.Errorf("Status = %q, want 500", ev.Status)
	}
	if ev.Revenue != 1200.5 {
		t.Errorf("Revenue = %v, want 1200.5", ev.Revenue)
	}
	if ev.OrderID != "ORD-1" || ev.ClientID != "c-1" || ev.UserID != "u-1" {
		t.Errorf("ids = %q/%q/%q", ev.OrderID, ev.ClientID, ev.UserID)
	}
	if len(ev.GoalsID) != 2 || ev.GoalsID[0] != "42" || ev.GoalsID[1] != "99" {
		t.Errorf("GoalsID = %v, want [42 99]", ev.GoalsID)
	}
	if ev.Params == nil {
		t.Error("Params should be initialized")
	}
}

func TestParseEvent_MissingFields(t *testing.T) {
	// Empty hit: no panics, zero values.
	ev := ParseEvent(Hit{})
	if !ev.EventTime.IsZero() || ev.PageURL != "" || ev.Status != "" || ev.Revenue != 0 {
		t.Errorf("expected zero event, got %+v", ev)
	}
	if ev.Params == nil {
		t.Error("Params should be initialized even for empty hit")
	}
}

func TestParseEvent_BadTimeIgnored(t *testing.T) {
	ev := ParseEvent(Hit{"event_time": "not-a-time"})
	if !ev.EventTime.IsZero() {
		t.Errorf("bad time should be ignored, got %v", ev.EventTime)
	}
}

func TestParseEvent_SingleGoal(t *testing.T) {
	ev := ParseEvent(Hit{"goals_id": "7"})
	if len(ev.GoalsID) != 1 || ev.GoalsID[0] != "7" {
		t.Errorf("GoalsID = %v, want [7]", ev.GoalsID)
	}
}

func newTestClient(t *testing.T, handler http.Handler) *MetrikaClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := &model.Counter{CounterID: "12345", OAuthToken: "tok-123"}
	return NewMetrikaClient(c, srv.URL, srv.URL)
}

func TestDoJSON_Success(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	var gotBody map[string]any
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"data":{"visits":42}}`))
	}))

	var resp struct {
		OK   bool `json:"ok"`
		Data struct {
			Visits float64 `json:"visits"`
		} `json:"data"`
	}
	if err := client.doJSON(context.Background(), "POST", "/12345/report", map[string]any{"date1": "a"}, &resp); err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	if gotAuth != "OAuth tok-123" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "OAuth tok-123")
	}
	if gotPath != "/12345/report" || gotMethod != "POST" {
		t.Errorf("path/method = %q/%q", gotPath, gotMethod)
	}
	if resp.Data.Visits != 42 {
		t.Errorf("decoded visits = %v, want 42", resp.Data.Visits)
	}
}

func TestDoJSON_NoBodyNoDst(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	if err := client.doJSON(context.Background(), "GET", "/ping", nil, nil); err != nil {
		t.Fatalf("doJSON (no body, no dst): %v", err)
	}
}

func TestDoJSON_RateLimit(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	err := client.doJSON(context.Background(), "POST", "/x", nil, nil)
	if err == nil {
		t.Fatal("expected rate-limit error, got nil")
	}
	if !strings.Contains(err.Error(), "rate limited") || !strings.Contains(err.Error(), "30") {
		t.Errorf("error = %q, want rate limited + retry-after", err)
	}
}

func TestDoJSON_RateLimitNoHeader(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	err := client.doJSON(context.Background(), "POST", "/x", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error = %v, want rate limited", err)
	}
}

func TestDoJSON_APIErrorWithMessage(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"code":400,"message":"invalid token"}}`))
	}))
	err := client.doJSON(context.Background(), "POST", "/x", nil, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid token") {
		t.Errorf("error = %q, want 400 + invalid token", err)
	}
}

func TestDoJSON_APIErrorNoMessage(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"code":500}}`))
	}))
	err := client.doJSON(context.Background(), "POST", "/x", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %v, want 500", err)
	}
}

func TestDoJSON_BadJSONResponse(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not-json`))
	}))
	var dst map[string]any
	err := client.doJSON(context.Background(), "GET", "/x", nil, &dst)
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Errorf("error = %v, want decode response error", err)
	}
}

func TestFetchEvents(t *testing.T) {
	var gotPath string
	var gotBody LogRequest
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Write([]byte(`{"total_hits":2,"hits":[{"event_time":"2026-08-01T10:00:00Z","page_url":"/a"},{"event_time":"2026-08-01T10:01:00Z","page_url":"/b"}]}`))
	}))

	since := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	hits, err := client.FetchEvents(context.Background(), since, 100)
	if err != nil {
		t.Fatalf("FetchEvents: %v", err)
	}
	if gotPath != "/12345/logs/hits" {
		t.Errorf("path = %q, want /12345/logs/hits", gotPath)
	}
	if gotBody.Limit != 100 {
		t.Errorf("limit = %d, want 100", gotBody.Limit)
	}
	if gotBody.StartTime != since.Format(time.RFC3339) {
		t.Errorf("start_time = %q, want %q", gotBody.StartTime, since.Format(time.RFC3339))
	}
	if len(gotBody.Fields) == 0 {
		t.Error("fields should not be empty")
	}
	if len(hits) != 2 || hits[0]["page_url"] != "/a" {
		t.Errorf("hits = %v", hits)
	}
}

func TestFetchEvents_ZeroSince(t *testing.T) {
	var gotBody LogRequest
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Write([]byte(`{"total_hits":0,"hits":[]}`))
	}))
	before := time.Now().Add(-time.Hour)
	hits, err := client.FetchEvents(context.Background(), time.Time{}, 10)
	if err != nil {
		t.Fatalf("FetchEvents: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("hits = %v, want empty", hits)
	}
	// Zero since should default to roughly the last hour.
	start, err := time.Parse(time.RFC3339, gotBody.StartTime)
	if err != nil {
		t.Fatalf("parse start_time: %v", err)
	}
	if start.After(before) {
		t.Errorf("default start %v is after %v (expected ~1h ago)", start, before)
	}
}

func TestFetchReport(t *testing.T) {
	var gotBody struct {
		Date1   string   `json:"date1"`
		Date2   string   `json:"date2"`
		Metrics []string `json:"metrics"`
		Limit   int      `json:"limit"`
	}
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Write([]byte(`{"data":[{"visits":10,"unique_visits":8}]}`))
	}))

	data, err := client.FetchReport(context.Background(), "2026-08-01", "2026-08-02", []string{"visits", "unique_visits"}, 5)
	if err != nil {
		t.Fatalf("FetchReport: %v", err)
	}
	if gotBody.Date1 != "2026-08-01" || gotBody.Date2 != "2026-08-02" || gotBody.Limit != 5 {
		t.Errorf("body = %+v", gotBody)
	}
	if len(gotBody.Metrics) != 2 || gotBody.Metrics[0] != "visits" {
		t.Errorf("metrics = %v", gotBody.Metrics)
	}
	if len(data) != 1 || data[0]["visits"] != float64(10) {
		t.Errorf("data = %v", data)
	}
}

func TestFetchEvents_ErrorPropagates(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":401,"message":"bad token"}}`))
	}))
	_, err := client.FetchEvents(context.Background(), time.Time{}, 10)
	if err == nil || !strings.Contains(err.Error(), "bad token") {
		t.Errorf("error = %v, want bad token", err)
	}
}
