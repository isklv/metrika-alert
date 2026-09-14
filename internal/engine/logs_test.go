package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

// logsStub imitates the Logs API: it records every request and answers with
// whatever the test queued.
type logsStub struct {
	mu       sync.Mutex
	requests []*http.Request
	handler  func(w http.ResponseWriter, r *http.Request)
	server   *httptest.Server
}

func newLogsStub(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *logsStub {
	t.Helper()
	s := &logsStub{handler: handler}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.Clone(r.Context()))
		s.mu.Unlock()
		s.handler(w, r)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *logsStub) paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.requests {
		out = append(out, r.Method+" "+r.URL.Path)
	}
	return out
}

func testCounter() *model.Counter {
	return &model.Counter{ID: 1, Name: "Магазин", CounterID: "12345678", OAuthToken: "y0_token", PollInterval: 60}
}

func testWindow() Window {
	return Window{
		From: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC),
	}
}

// The whole point of the rewrite: the paths must be the ones Metrika documents.
func TestLogsClientUsesDocumentedEndpoints(t *testing.T) {
	stub := newLogsStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/logrequests/evaluate"):
			fmt.Fprint(w, `{"log_request_evaluation":{"possible":true,"max_possible_day_quantity":7}}`)
		case strings.HasSuffix(r.URL.Path, "/logrequests"):
			fmt.Fprint(w, `{"log_request":{"request_id":555,"status":"created"}}`)
		case strings.HasSuffix(r.URL.Path, "/clean"):
			fmt.Fprint(w, `{"log_request":{"request_id":555,"status":"cleaned_by_user"}}`)
		default:
			fmt.Fprint(w, `{"log_request":{"request_id":555,"status":"processed","parts":[{"part_number":0,"size":10}]}}`)
		}
	})

	c := NewLogsClient(testCounter(), stub.server.URL)
	ctx := context.Background()

	if _, err := c.Evaluate(ctx, testWindow()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if _, err := c.Create(ctx, testWindow()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := c.Get(ctx, 555); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := c.Clean(ctx, 555); err != nil {
		t.Fatalf("Clean: %v", err)
	}

	want := []string{
		"GET /management/v1/counter/12345678/logrequests/evaluate",
		"POST /management/v1/counter/12345678/logrequests",
		"GET /management/v1/counter/12345678/logrequest/555",
		"POST /management/v1/counter/12345678/logrequest/555/clean",
	}
	got := stub.paths()
	if len(got) != len(want) {
		t.Fatalf("got %d requests, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCreateSendsWindowAndFields(t *testing.T) {
	var got *http.Request
	stub := newLogsStub(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		fmt.Fprint(w, `{"log_request":{"request_id":7,"status":"created"}}`)
	})

	c := NewLogsClient(testCounter(), stub.server.URL)
	if _, err := c.Create(context.Background(), testWindow()); err != nil {
		t.Fatalf("Create: %v", err)
	}

	q := got.URL.Query()
	if q.Get("source") != "hits" {
		t.Errorf("source = %q, want hits", q.Get("source"))
	}
	// The API expects ISO 8601, not the "2006-01-02 15:00" the old code sent.
	if q.Get("date1") != "2026-09-10T00:00:00Z" {
		t.Errorf("date1 = %q", q.Get("date1"))
	}
	if q.Get("date2") != "2026-09-11T00:00:00Z" {
		t.Errorf("date2 = %q", q.Get("date2"))
	}
	fields := q.Get("fields")
	for _, want := range []string{"ym:pv:dateTime", "ym:pv:URL", "ym:pv:goalsID", "ym:pv:httpError"} {
		if !strings.Contains(fields, want) {
			t.Errorf("fields %q is missing %s", fields, want)
		}
	}
	if got.Header.Get("Authorization") != "OAuth y0_token" {
		t.Errorf("Authorization = %q", got.Header.Get("Authorization"))
	}
}

func TestLogRequestLifecycleStates(t *testing.T) {
	tests := []struct {
		status   string
		ready    bool
		terminal bool
	}{
		{StatusCreated, false, false},
		{StatusAwaitingRetry, false, false},
		{StatusProcessed, true, false},
		{StatusCanceled, false, true},
		{StatusFailed, false, true},
		{StatusCleanedByUser, false, true},
		{StatusCleanedTooOld, false, true},
	}
	for _, tc := range tests {
		r := &LogRequest{Status: tc.status}
		if r.Ready() != tc.ready {
			t.Errorf("%s: Ready() = %v, want %v", tc.status, r.Ready(), tc.ready)
		}
		if r.Terminal() != tc.terminal {
			t.Errorf("%s: Terminal() = %v, want %v", tc.status, r.Terminal(), tc.terminal)
		}
	}
}

func TestDownloadEventsWalksEveryPart(t *testing.T) {
	stub := newLogsStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/part/0/download"):
			fmt.Fprint(w, "ym:pv:dateTime\tym:pv:URL\n2026-09-10 10:00:00\t/a\n")
		case strings.HasSuffix(r.URL.Path, "/part/1/download"):
			fmt.Fprint(w, "ym:pv:dateTime\tym:pv:URL\n2026-09-10 11:00:00\t/b\n")
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	req := &LogRequest{RequestID: 9}
	req.Parts = append(req.Parts, struct {
		PartNumber int   `json:"part_number"`
		Size       int64 `json:"size"`
	}{0, 10}, struct {
		PartNumber int   `json:"part_number"`
		Size       int64 `json:"size"`
	}{1, 10})

	var urls []string
	c := NewLogsClient(testCounter(), stub.server.URL)
	err := c.DownloadEvents(context.Background(), req, func(ev *model.MetrikaEvent) error {
		urls = append(urls, ev.PageURL)
		return nil
	})
	if err != nil {
		t.Fatalf("DownloadEvents: %v", err)
	}
	if len(urls) != 2 || urls[0] != "/a" || urls[1] != "/b" {
		t.Errorf("events = %v, want both parts", urls)
	}
}

// ---- TSV parsing ----

func parseAll(t *testing.T, tsv string) []*model.MetrikaEvent {
	t.Helper()
	var out []*model.MetrikaEvent
	if err := ParseTSV(strings.NewReader(tsv), func(ev *model.MetrikaEvent) error {
		out = append(out, ev)
		return nil
	}); err != nil {
		t.Fatalf("ParseTSV: %v", err)
	}
	return out
}

func TestParseTSVMapsFieldsOntoEvents(t *testing.T) {
	tsv := "ym:pv:dateTime\tym:pv:URL\tym:pv:title\tym:pv:httpError\tym:pv:goalsID\tym:pv:purchaseRevenue\tym:pv:purchaseID\tym:pv:clientID\n" +
		"2026-09-10 14:23:05\thttps://shop.example/checkout\tОформление\t500\t[42,77]\t[1200.5]\t[ORD-1]\t1553980932\n"

	events := parseAll(t, tsv)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]

	if ev.EventTime.Format("2006-01-02 15:04:05") != "2026-09-10 14:23:05" {
		t.Errorf("EventTime = %v", ev.EventTime)
	}
	if ev.PageURL != "https://shop.example/checkout" {
		t.Errorf("PageURL = %q", ev.PageURL)
	}
	if ev.Title != "Оформление" {
		t.Errorf("Title = %q", ev.Title)
	}
	if ev.Status != "500" {
		t.Errorf("Status = %q", ev.Status)
	}
	if len(ev.GoalsID) != 2 || ev.GoalsID[0] != "42" || ev.GoalsID[1] != "77" {
		t.Errorf("GoalsID = %v", ev.GoalsID)
	}
	if ev.Revenue != 1200.5 {
		t.Errorf("Revenue = %v", ev.Revenue)
	}
	if ev.OrderID != "ORD-1" {
		t.Errorf("OrderID = %q", ev.OrderID)
	}
	if ev.ClientID != "1553980932" {
		t.Errorf("ClientID = %q", ev.ClientID)
	}
}

// Column order comes from the payload's own header, so a reordered export
// still parses correctly.
func TestParseTSVFollowsHeaderOrder(t *testing.T) {
	tsv := "ym:pv:URL\tym:pv:httpError\tym:pv:dateTime\n/checkout\t404\t2026-09-10 09:00:00\n"

	ev := parseAll(t, tsv)[0]
	if ev.PageURL != "/checkout" || ev.Status != "404" {
		t.Errorf("event = %+v", ev)
	}
	if ev.EventTime.IsZero() {
		t.Error("EventTime was not parsed")
	}
}

// An event may carry several purchases; a revenue trigger asks about the whole
// event, so they sum.
func TestParseTSVSumsMultiplePurchases(t *testing.T) {
	tsv := "ym:pv:purchaseRevenue\n[100.25,200.75,1000]\n"
	if got := parseAll(t, tsv)[0].Revenue; got != 1301 {
		t.Errorf("Revenue = %v, want 1301", got)
	}
}

func TestParseTSVHandlesAbsentValues(t *testing.T) {
	tsv := "ym:pv:URL\tym:pv:httpError\tym:pv:goalsID\tym:pv:purchaseRevenue\n" +
		`/ok` + "\t" + `\N` + "\t[]\t" + `\N` + "\n"

	ev := parseAll(t, tsv)[0]
	if ev.Status != "" {
		t.Errorf(`\N should leave the field empty, got %q`, ev.Status)
	}
	if len(ev.GoalsID) != 0 {
		t.Errorf("empty array should yield no goals, got %v", ev.GoalsID)
	}
	if ev.Revenue != 0 {
		t.Errorf("Revenue = %v, want 0", ev.Revenue)
	}
}

// A page title containing a tab would otherwise split the row and shift every
// column after it.
func TestParseTSVUnescapesControlCharacters(t *testing.T) {
	tsv := "ym:pv:title\tym:pv:URL\n" + `Корзина\tи оплата` + "\t/cart\n"

	ev := parseAll(t, tsv)[0]
	if ev.Title != "Корзина\tи оплата" {
		t.Errorf("Title = %q, want the tab unescaped", ev.Title)
	}
	if ev.PageURL != "/cart" {
		t.Errorf("PageURL = %q — the column after an escaped tab shifted", ev.PageURL)
	}
}

// A URL with a Windows path or a regex keeps its backslash rather than losing it.
func TestParseTSVKeepsUnknownEscapes(t *testing.T) {
	tsv := "ym:pv:URL\n" + `/search?q=a\db` + "\n"
	if got := parseAll(t, tsv)[0].PageURL; got != `/search?q=a\db` {
		t.Errorf("PageURL = %q", got)
	}
}

// No hits in the window is a legitimate answer, not a parse failure.
func TestParseTSVAcceptsEmptyExport(t *testing.T) {
	if got := parseAll(t, ""); len(got) != 0 {
		t.Errorf("got %d events from an empty export", len(got))
	}
	if got := parseAll(t, "ym:pv:dateTime\tym:pv:URL\n"); len(got) != 0 {
		t.Errorf("got %d events from a header-only export", len(got))
	}
}

// Fields outside the known set stay reachable, so a trigger can name one.
func TestParseTSVKeepsUnknownFieldsAddressable(t *testing.T) {
	tsv := "ym:pv:URL\tym:pv:referer\n/a\thttps://ya.ru\n"
	ev := parseAll(t, tsv)[0]
	if ev.Params["ym:pv:referer"] != "https://ya.ru" {
		t.Errorf("Params = %v", ev.Params)
	}
}

func TestParseTSVStopsOnConsumerError(t *testing.T) {
	tsv := "ym:pv:URL\n/a\n/b\n/c\n"
	count := 0
	err := ParseTSV(strings.NewReader(tsv), func(ev *model.MetrikaEvent) error {
		count++
		return fmt.Errorf("boom")
	})
	if err == nil {
		t.Fatal("expected the consumer error to propagate")
	}
	if count != 1 {
		t.Errorf("consumer called %d times, want 1", count)
	}
}
