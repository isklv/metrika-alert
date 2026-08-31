package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isklv/metrika-alert/internal/engine"
	"github.com/isklv/metrika-alert/internal/model"
)

func newTestServer(t *testing.T) (*Server, *model.DB, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	db, err := model.OpenDB(filepath.Join(dir, "api.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	poller := engine.NewPoller(db, &engine.MetrikaConfig{BaseURL: "http://unused"}, func(int64, []*model.MetrikaEvent) error { return nil })
	reporter := engine.NewReporter(db, &engine.MetrikaConfig{BaseURL: "http://unused"}, nil)
	s := NewServer(db, reporter, poller, "127.0.0.1:0")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)
	return s, db, ts
}

// do performs a request and returns the status code and raw body.
func do(t *testing.T, method, url string, body []byte) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
}

func mustCounter(t *testing.T, db *model.DB) int64 {
	t.Helper()
	c := &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}
	if err := db.CreateCounter(context.Background(), c); err != nil {
		t.Fatalf("create counter: %v", err)
	}
	return c.ID
}

func TestCounters_CRUD(t *testing.T) {
	_, db, ts := newTestServer(t)

	// GET empty -> JSON array [].
	code, raw := do(t, "GET", ts.URL+"/api/counters", nil)
	if code != 200 {
		t.Fatalf("GET counters = %d", code)
	}
	var list []model.Counter
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("unmarshal counters: %v (%s)", err, raw)
	}
	if len(list) != 0 {
		t.Errorf("counters = %d, want 0", len(list))
	}

	// POST missing fields -> 400.
	code, _ = do(t, "POST", ts.URL+"/api/counters", []byte(`{"name":"x"}`))
	if code != 400 {
		t.Fatalf("POST incomplete = %d, want 400", code)
	}

	// POST valid (default poll interval) -> 201 + object.
	code, raw = do(t, "POST", ts.URL+"/api/counters", []byte(`{"name":"Shop","counter_id":"12345","oauth_token":"tok"}`))
	if code != 201 {
		t.Fatalf("POST counter = %d, want 201", code)
	}
	var created model.Counter
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("unmarshal created: %v", err)
	}
	if created.Name != "Shop" || created.CounterID != "12345" {
		t.Errorf("created = %+v", created)
	}
	if created.PollInterval != 60 {
		t.Errorf("default poll interval = %d, want 60", created.PollInterval)
	}

	// GET now returns 1.
	code, raw = do(t, "GET", ts.URL+"/api/counters", nil)
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("counters after create = %d, want 1", len(list))
	}

	// Bad JSON -> 400.
	code, _ = do(t, "POST", ts.URL+"/api/counters", []byte(`{bad`))
	if code != 400 {
		t.Errorf("POST bad json = %d, want 400", code)
	}

	// Wrong method -> 405.
	code, _ = do(t, "DELETE", ts.URL+"/api/counters", nil)
	if code != 405 {
		t.Errorf("DELETE counters = %d, want 405", code)
	}

	_ = db
}

func TestMonitors(t *testing.T) {
	_, db, ts := newTestServer(t)
	mustCounter(t, db)

	// GET without counter_id -> 400.
	code, _ := do(t, "GET", ts.URL+"/api/monitors", nil)
	if code != 400 {
		t.Fatalf("GET monitors no id = %d, want 400", code)
	}

	// POST via query param counter_id -> 201.
	code, raw := do(t, "POST", ts.URL+"/api/monitors?counter_id=1", []byte(`{"name":"Checkout","url_pattern":"/checkout*"}`))
	if code != 201 {
		t.Fatalf("POST monitor = %d, want 201", code)
	}
	var m model.PageMonitor
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal monitor: %v", err)
	}
	if m.Name != "Checkout" || m.URLPattern != "/checkout*" {
		t.Errorf("monitor = %+v", m)
	}
	if len(m.Metrics) == 0 {
		t.Errorf("default metrics missing: %+v", m)
	}

	// POST missing url_pattern -> 400.
	code, _ = do(t, "POST", ts.URL+"/api/monitors?counter_id=1", []byte(`{"name":"x"}`))
	if code != 400 {
		t.Errorf("POST monitor incomplete = %d, want 400", code)
	}

	// GET with counter_id -> array of 1.
	code, raw = do(t, "GET", ts.URL+"/api/monitors?counter_id=1", nil)
	if code != 200 {
		t.Fatalf("GET monitors = %d", code)
	}
	var mons []model.PageMonitor
	if err := json.Unmarshal(raw, &mons); err != nil {
		t.Fatalf("unmarshal monitors: %v", err)
	}
	if len(mons) != 1 {
		t.Errorf("monitors = %d, want 1", len(mons))
	}
}

func TestTriggers(t *testing.T) {
	_, db, ts := newTestServer(t)
	mustCounter(t, db)

	// GET without counter_id -> 400.
	code, _ := do(t, "GET", ts.URL+"/api/triggers", nil)
	if code != 400 {
		t.Fatalf("GET triggers no id = %d, want 400", code)
	}

	// POST via body counter_id (parseBodyInt64 path) -> 201.
	code, raw := do(t, "POST", ts.URL+"/api/triggers", []byte(`{"counter_id":1,"name":"500s","condition":"status_code == 500"}`))
	if code != 201 {
		t.Fatalf("POST trigger = %d, want 201", code)
	}
	var tr model.Trigger
	if err := json.Unmarshal(raw, &tr); err != nil {
		t.Fatalf("unmarshal trigger: %v", err)
	}
	// Defaults applied.
	if tr.Threshold != 1 || tr.Window != 30 || tr.Cooldown != 60 {
		t.Errorf("trigger defaults = %+v", tr)
	}

	// POST missing condition -> 400.
	code, _ = do(t, "POST", ts.URL+"/api/triggers", []byte(`{"counter_id":1,"name":"x"}`))
	if code != 400 {
		t.Errorf("POST trigger incomplete = %d, want 400", code)
	}

	// GET with counter_id -> array of 1.
	code, raw = do(t, "GET", ts.URL+"/api/triggers?counter_id=1", nil)
	if code != 200 {
		t.Fatalf("GET triggers = %d", code)
	}
	var trigs []model.Trigger
	if err := json.Unmarshal(raw, &trigs); err != nil {
		t.Fatalf("unmarshal triggers: %v", err)
	}
	if len(trigs) != 1 {
		t.Errorf("triggers = %d, want 1", len(trigs))
	}
}

func TestAlertActions_GET(t *testing.T) {
	_, db, ts := newTestServer(t)
	if err := db.CreateAlertAction(context.Background(), &model.AlertAction{Name: "hook", Type: "webhook", URL: "http://example.com"}); err != nil {
		t.Fatalf("create action: %v", err)
	}
	code, raw := do(t, "GET", ts.URL+"/api/alert-actions", nil)
	if code != 200 {
		t.Fatalf("GET alert-actions = %d", code)
	}
	var actions []model.AlertAction
	if err := json.Unmarshal(raw, &actions); err != nil {
		t.Fatalf("unmarshal actions: %v", err)
	}
	if len(actions) != 1 {
		t.Errorf("actions = %d, want 1", len(actions))
	}
}

func TestAlerts_GET(t *testing.T) {
	_, db, ts := newTestServer(t)
	cid := mustCounter(t, db)
	trig := &model.Trigger{CounterID: cid, Name: "t", Condition: "x", Threshold: 1, Window: 30, Cooldown: 60, Enabled: true}
	if err := db.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if err := db.CreateAlert(context.Background(), &model.Alert{TriggerID: trig.ID, CounterID: cid, Title: "T", Message: "M", EventCount: 2}); err != nil {
		t.Fatalf("create alert: %v", err)
	}

	code, raw := do(t, "GET", ts.URL+"/api/alerts?counter_id=1", nil)
	if code != 200 {
		t.Fatalf("GET alerts = %d", code)
	}
	var alerts []model.Alert
	if err := json.Unmarshal(raw, &alerts); err != nil {
		t.Fatalf("unmarshal alerts: %v", err)
	}
	if len(alerts) != 1 {
		t.Errorf("alerts = %d, want 1", len(alerts))
	}
}

func TestReports(t *testing.T) {
	_, db, ts := newTestServer(t)
	mustCounter(t, db)

	// GET without counter_id -> 400.
	code, _ := do(t, "GET", ts.URL+"/api/reports", nil)
	if code != 400 {
		t.Fatalf("GET reports no id = %d, want 400", code)
	}

	// GET with counter_id -> empty array.
	code, raw := do(t, "GET", ts.URL+"/api/reports?counter_id=1", nil)
	if code != 200 {
		t.Fatalf("GET reports = %d", code)
	}
	var snaps []model.ReportSnapshot
	if err := json.Unmarshal(raw, &snaps); err != nil {
		t.Fatalf("unmarshal snapshots: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("snapshots = %d, want 0", len(snaps))
	}

	// POST without counter_id -> RunReports (no counters to fetch) -> 202.
	code, _ = do(t, "POST", ts.URL+"/api/reports", nil)
	if code != 202 {
		t.Errorf("POST reports (all) = %d, want 202", code)
	}

	// POST with counter_id -> ReportCounter fails (no real API) -> 500.
	code, _ = do(t, "POST", ts.URL+"/api/reports?counter_id=1", nil)
	if code != 500 {
		t.Errorf("POST reports (one) = %d, want 500 (fetch fails without API)", code)
	}
}

func TestEvents(t *testing.T) {
	_, db, ts := newTestServer(t)
	mustCounter(t, db)

	code, _ := do(t, "GET", ts.URL+"/api/events", nil)
	if code != 400 {
		t.Fatalf("GET events no id = %d, want 400", code)
	}

	code, raw := do(t, "GET", ts.URL+"/api/events?counter_id=1", nil)
	if code != 200 {
		t.Fatalf("GET events = %d", code)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal events: %v", err)
	}
	if body["note"] == "" || body["alerts"] != float64(0) {
		t.Errorf("events body = %v", body)
	}
}

func TestStatus(t *testing.T) {
	_, db, ts := newTestServer(t)
	mustCounter(t, db)

	code, raw := do(t, "GET", ts.URL+"/api/status", nil)
	if code != 200 {
		t.Fatalf("GET status = %d", code)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if body["status"] != "running" || body["counters"] != float64(1) || body["last_alert"] != "never" {
		t.Errorf("status = %v", body)
	}
}

func TestParseOptionalInt64(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"5", 5},
		{"  42 ", 42},
		{"abc", 0},
		{"-3", -3},
	}
	for _, tt := range tests {
		if got := parseOptionalInt64(tt.in); got != tt.want {
			t.Errorf("parseOptionalInt64(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestParseBodyInt64(t *testing.T) {
	body := `{"counter_id": 7, "other": "x"}`
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	if got := parseBodyInt64(r, "counter_id"); got != 7 {
		t.Errorf("parseBodyInt64 = %d, want 7", got)
	}
	r2 := httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
	if got := parseBodyInt64(r2, "counter_id"); got != 0 {
		t.Errorf("parseBodyInt64 missing = %d, want 0", got)
	}
	r3 := httptest.NewRequest("POST", "/", strings.NewReader(`bad`))
	if got := parseBodyInt64(r3, "counter_id"); got != 0 {
		t.Errorf("parseBodyInt64 bad json = %d, want 0", got)
	}
}
