package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

type capturedAlert struct {
	title   string
	message string
}

type fakeRouter struct {
	mu     sync.Mutex
	alerts []capturedAlert
}

func (f *fakeRouter) Alert(_ context.Context, _ int64, title, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alerts = append(f.alerts, capturedAlert{title, message})
	return nil
}

func (f *fakeRouter) last() capturedAlert {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.alerts) == 0 {
		return capturedAlert{}
	}
	return f.alerts[len(f.alerts)-1]
}

// reportFake answers /stat/v1/data from a map keyed by the date1 parameter, so
// a test can give each comparison period its own figures.
type reportFake struct {
	mu       sync.Mutex
	byPeriod map[string]string
	goals    string
	queries  []url.Values
}

func (f *reportFake) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.queries = append(f.queries, r.URL.Query())

		if strings.HasSuffix(r.URL.Path, "/goals") {
			fmt.Fprint(w, f.goals)
			return
		}
		if body, ok := f.byPeriod[r.URL.Query().Get("date1")]; ok {
			fmt.Fprint(w, body)
			return
		}
		fmt.Fprint(w, `{"data":[],"totals":[]}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (f *reportFake) periods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, q := range f.queries {
		if d := q.Get("date1"); d != "" {
			out = append(out, d+"→"+q.Get("date2"))
		}
	}
	return out
}

// totals builds a stat response in SummaryMetrics order.
func totals(visits, users, pageviews, bounce, depth, duration, goals float64) string {
	return fmt.Sprintf(`{"data":[{"dimensions":[],"metrics":[%g,%g,%g,%g,%g,%g,%g]}],"totals":[%g,%g,%g,%g,%g,%g,%g],"total_rows":1}`,
		visits, users, pageviews, bounce, depth, duration, goals,
		visits, users, pageviews, bounce, depth, duration, goals)
}

func newReporterHarness(t *testing.T, fake *reportFake) (*Reporter, *fakeRouter, *model.Counter) {
	t.Helper()
	db, err := model.OpenDB(filepath.Join(t.TempDir(), "report.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	counter := testCounter()
	if err := db.CreateCounter(context.Background(), counter); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}

	router := &fakeRouter{}
	return NewReporter(db, &MetrikaConfig{BaseURL: fake.serve(t)}, router), router, counter
}

// The old reporter compared a point in time against a full day, so every
// morning report showed a decline. Periods must be like for like.
func TestReportComparesEquivalentPeriods(t *testing.T) {
	fake := &reportFake{
		byPeriod: map[string]string{
			"today":     totals(1000, 800, 3000, 25, 3, 180, 40),
			"yesterday": totals(800, 640, 2400, 30, 2.8, 150, 30),
			"7daysAgo":  totals(1250, 1000, 3750, 20, 3.2, 200, 50),
			"30daysAgo": totals(500, 400, 1500, 35, 2.5, 120, 10),
		},
		goals: `{"goals":[]}`,
	}
	reporter, router, counter := newReporterHarness(t, fake)

	client := NewReportClient(counter, fake.serve(t))
	if err := reporter.ReportCounter(context.Background(), counter, client, time.Now()); err != nil {
		t.Fatalf("ReportCounter: %v", err)
	}

	for _, want := range []string{"today→today", "yesterday→yesterday", "7daysAgo→7daysAgo", "30daysAgo→30daysAgo"} {
		found := false
		for _, got := range fake.periods() {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("period %s was never requested; asked for %v", want, fake.periods())
		}
	}

	card := router.last().message
	if !strings.Contains(card, "1000") {
		t.Errorf("card is missing today's visits:\n%s", card)
	}
	// 1000 against 800 yesterday is +25%.
	if !strings.Contains(card, "▲25%") {
		t.Errorf("card is missing the change against yesterday:\n%s", card)
	}
	// 1000 against 1250 a week ago is -20%.
	if !strings.Contains(card, "▼20%") {
		t.Errorf("card is missing the change against last week:\n%s", card)
	}
}

// A counter with no data a month ago must still produce today's report.
func TestReportSurvivesMissingComparisonPeriods(t *testing.T) {
	fake := &reportFake{
		byPeriod: map[string]string{"today": totals(42, 30, 100, 10, 2, 90, 5)},
		goals:    `{"goals":[]}`,
	}
	reporter, router, counter := newReporterHarness(t, fake)

	client := NewReportClient(counter, fake.serve(t))
	if err := reporter.ReportCounter(context.Background(), counter, client, time.Now()); err != nil {
		t.Fatalf("ReportCounter: %v", err)
	}
	if card := router.last().message; !strings.Contains(card, "42") {
		t.Errorf("report was not built:\n%s", card)
	}
}

func TestReportStatesWhenThereIsNoData(t *testing.T) {
	fake := &reportFake{byPeriod: map[string]string{}, goals: `{"goals":[]}`}
	reporter, router, counter := newReporterHarness(t, fake)

	client := NewReportClient(counter, fake.serve(t))
	if err := reporter.ReportCounter(context.Background(), counter, client, time.Now()); err != nil {
		t.Fatalf("ReportCounter: %v", err)
	}
	if card := router.last().message; !strings.Contains(card, "данных пока нет") {
		t.Errorf("card = %q", card)
	}
}

func TestReportBreaksDownGoalsByName(t *testing.T) {
	fake := &reportFake{
		byPeriod: map[string]string{"today": totals(100, 80, 300, 20, 3, 120, 15)},
		goals:    `{"goals":[{"id":42,"name":"Покупка"},{"id":77,"name":"Регистрация"}]}`,
	}
	reporter, router, counter := newReporterHarness(t, fake)

	// Goal reaches come back on the same today query; the stub answers every
	// today request with the same totals, so both goals report a figure.
	client := NewReportClient(counter, fake.serve(t))
	if err := reporter.ReportCounter(context.Background(), counter, client, time.Now()); err != nil {
		t.Fatalf("ReportCounter: %v", err)
	}

	card := router.last().message
	if !strings.Contains(card, "Покупка") {
		t.Errorf("goal names are missing from the card:\n%s", card)
	}
}

// A token without management access should still yield a report.
func TestReportWithoutGoalAccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/goals") {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"Access denied"}`)
			return
		}
		fmt.Fprint(w, totals(100, 80, 300, 20, 3, 120, 15))
	}))
	defer srv.Close()

	db, err := model.OpenDB(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	counter := testCounter()
	if err := db.CreateCounter(context.Background(), counter); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}
	router := &fakeRouter{}
	reporter := NewReporter(db, &MetrikaConfig{BaseURL: srv.URL}, router)

	if err := reporter.ReportCounter(context.Background(), counter, NewReportClient(counter, srv.URL), time.Now()); err != nil {
		t.Fatalf("ReportCounter: %v", err)
	}
	if card := router.last().message; !strings.Contains(card, "100") {
		t.Errorf("report was lost when goals were unavailable:\n%s", card)
	}
}

func TestReportSnapshotIsStored(t *testing.T) {
	fake := &reportFake{
		byPeriod: map[string]string{"today": totals(1000, 800, 3000, 25, 3.5, 180, 40)},
		goals:    `{"goals":[]}`,
	}
	reporter, _, counter := newReporterHarness(t, fake)
	now := time.Date(2026, 9, 12, 14, 0, 0, 0, time.Local)

	client := NewReportClient(counter, fake.serve(t))
	if err := reporter.ReportCounter(context.Background(), counter, client, now); err != nil {
		t.Fatalf("ReportCounter: %v", err)
	}

	snapshot, err := reporter.db.GetSnapshot(context.Background(), counter.ID, nil, "hour", "2026-09-12T14")
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if snapshot.Visits != 1000 || snapshot.UniqueVisits != 800 {
		t.Errorf("snapshot = %+v", snapshot)
	}
	// bounceRate is a percentage; the snapshot holds an absolute count.
	if snapshot.Bounces != 250 {
		t.Errorf("bounces = %d, want 25%% of 1000", snapshot.Bounces)
	}
	if snapshot.Depth != 3.5 {
		t.Errorf("depth = %v", snapshot.Depth)
	}
}

func TestFormatDelta(t *testing.T) {
	tests := []struct {
		current, previous float64
		want              string
	}{
		{125, 100, "▲25%"},
		{75, 100, "▼25%"},
		{100, 100, "≈"},
		{1001, 1000, "≈"}, // noise below half a percent reads as flat
		{200, 100, "▲100%"},
	}
	for _, tc := range tests {
		if got := formatDelta(tc.current, tc.previous); got != tc.want {
			t.Errorf("formatDelta(%g, %g) = %q, want %q", tc.current, tc.previous, got, tc.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := map[float64]string{0: "0:00", 59: "0:59", 60: "1:00", 185: "3:05", 3600: "60:00"}
	for seconds, want := range tests {
		if got := formatDuration(seconds); got != want {
			t.Errorf("formatDuration(%g) = %q, want %q", seconds, got, want)
		}
	}
}
