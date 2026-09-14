package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isklv/metrika-alert/internal/engine"
	"github.com/isklv/metrika-alert/internal/model"
)

func testServer(t *testing.T, authToken string) (*Server, *model.DB) {
	t.Helper()
	db, err := model.OpenDB(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	metrika := &engine.MetrikaConfig{BaseURL: "https://api.example"}
	s := NewServer(db, engine.NewReporter(db, metrika, nil), engine.NewPoller(db, metrika, nil), Config{
		ListenAddr: ":0",
		AuthToken:  authToken,
		Metrika:    metrika,
	})
	return s, db
}

func do(s *Server, method, target, body, token string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	return w
}

// GET /api/counters returns every counter, and each one carries the Metrika
// OAuth token. Serialising it would hand out credentials to anyone who can
// reach the port.
func TestCounterResponseNeverLeaksOAuthToken(t *testing.T) {
	s, db := testServer(t, "")
	const secret = "y0_AgAAAAsecret"
	if err := db.CreateCounter(context.Background(), &model.Counter{
		Name: "Магазин", CounterID: "12345678", OAuthToken: secret, PollInterval: 60,
	}); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}

	w := do(s, http.MethodGet, "/api/counters", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Fatalf("response leaks the OAuth token: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "12345678") {
		t.Errorf("response is missing the counter: %s", w.Body.String())
	}
}

func TestCreateCounterAlsoHidesTheToken(t *testing.T) {
	s, _ := testServer(t, "")
	const secret = "y0_AgAAAAsecret"

	w := do(s, http.MethodPost, "/api/counters",
		`{"name":"Магазин","counter_id":"12345678","oauth_token":"`+secret+`"}`, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Errorf("creation response echoes the token: %s", w.Body.String())
	}
}

func TestAuthTokenGatesTheAPI(t *testing.T) {
	s, _ := testServer(t, "s3cret")

	if w := do(s, http.MethodGet, "/api/counters", "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", w.Code)
	}
	if w := do(s, http.MethodGet, "/api/counters", "", "wrong"); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", w.Code)
	}
	if w := do(s, http.MethodGet, "/api/counters", "", "s3cret"); w.Code != http.StatusOK {
		t.Errorf("correct token: status = %d, want 200", w.Code)
	}
}

// The health probe has to stay reachable for container orchestration.
func TestHealthzNeedsNoToken(t *testing.T) {
	s, _ := testServer(t, "s3cret")
	if w := do(s, http.MethodGet, "/healthz", "", ""); w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// counter_id in the body used to drain the reader before the handler decoded
// it, so every such request failed with EOF.
func TestCreateTriggerReadsCounterIDFromBody(t *testing.T) {
	s, db := testServer(t, "")
	c := &model.Counter{Name: "n", CounterID: "1", OAuthToken: "t", PollInterval: 60}
	if err := db.CreateCounter(context.Background(), c); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}

	w := do(s, http.MethodPost, "/api/triggers",
		`{"counter_id":1,"name":"Визиты","metric":"visits","direction":"drop","deviation_percent":40}`, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}

	var got model.Trigger
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CounterID != 1 || got.Name != "Визиты" || got.DeviationPct != 40 {
		t.Errorf("created trigger = %+v", got)
	}
	if got.Metric != "visits" || got.Direction != "drop" {
		t.Errorf("rule = %+v", got)
	}

	triggers, err := db.ListTriggers(context.Background(), 1)
	if err != nil || len(triggers) != 1 {
		t.Fatalf("ListTriggers = %v, %v", triggers, err)
	}
}

func TestAlertActionsExposeVKTeamsTargets(t *testing.T) {
	s, db := testServer(t, "")
	if err := db.CreateAlertAction(context.Background(), &model.AlertAction{
		Name: "vk", Type: "vkteams", Target: "team@corp.ru",
	}); err != nil {
		t.Fatalf("CreateAlertAction: %v", err)
	}

	w := do(s, http.MethodGet, "/api/alert-actions", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "team@corp.ru") {
		t.Errorf("body = %s", w.Body.String())
	}
}

// List endpoints must answer with [] rather than null, so a client can iterate
// the result without a special case for "no rows yet".
func TestEmptyListsAreArraysNotNull(t *testing.T) {
	s, db := testServer(t, "")
	if err := db.CreateCounter(context.Background(), &model.Counter{
		Name: "n", CounterID: "1", OAuthToken: "t", PollInterval: 60,
	}); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}

	for _, target := range []string{"/api/counters", "/api/alert-actions", "/api/alerts", "/api/triggers?counter_id=1"} {
		w := do(s, http.MethodGet, target, "", "")
		if w.Code != http.StatusOK {
			t.Errorf("%s: status = %d: %s", target, w.Code, w.Body)
			continue
		}
		if body := strings.TrimSpace(w.Body.String()); body == "null" {
			t.Errorf("%s returned null, want []", target)
		}
	}
}

func TestCreateTriggerWithURLScope(t *testing.T) {
	s, db := testServer(t, "")
	if err := db.CreateCounter(context.Background(), &model.Counter{
		Name: "n", CounterID: "1", OAuthToken: "t", PollInterval: 60,
	}); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}

	w := do(s, http.MethodPost, "/api/triggers",
		`{"counter_id":1,"name":"Чекаут","metric":"visits","direction":"drop","deviation_percent":40,"url_filter":"/checkout"}`, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}

	var got model.Trigger
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.URLFilter != "/checkout" {
		t.Errorf("url_filter = %q", got.URLFilter)
	}
	// An omitted url_match defaults to substring rather than failing.
	if got.URLMatch != "contains" {
		t.Errorf("url_match = %q, want contains", got.URLMatch)
	}
}

func TestCreateTriggerRejectsBadScope(t *testing.T) {
	s, db := testServer(t, "")
	if err := db.CreateCounter(context.Background(), &model.Counter{
		Name: "n", CounterID: "1", OAuthToken: "t", PollInterval: 60,
	}); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}

	tests := map[string]string{
		"unknown match": `{"counter_id":1,"name":"X","metric":"visits","deviation_percent":40,"url_filter":"/a","url_match":"glob"}`,
		"broken regexp": `{"counter_id":1,"name":"X","metric":"visits","deviation_percent":40,"url_filter":"^/a[","url_match":"regexp"}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if w := do(s, http.MethodPost, "/api/triggers", body, ""); w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", w.Code, w.Body)
			}
		})
	}

	if triggers, _ := db.ListTriggers(context.Background(), 1); len(triggers) != 0 {
		t.Errorf("a rejected scope created %d rule(s)", len(triggers))
	}
}
