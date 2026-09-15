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

// wholeCounter is the implicit report an install without configured ones gets.
func wholeCounter(c *model.Counter) *model.Report {
	return &model.Report{CounterID: c.ID, Name: c.Name, Enabled: true}
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
	if err := reporter.ReportOne(context.Background(), counter, wholeCounter(counter), client, time.Now()); err != nil {
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
	if err := reporter.ReportOne(context.Background(), counter, wholeCounter(counter), client, time.Now()); err != nil {
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
	if err := reporter.ReportOne(context.Background(), counter, wholeCounter(counter), client, time.Now()); err != nil {
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
	if err := reporter.ReportOne(context.Background(), counter, wholeCounter(counter), client, time.Now()); err != nil {
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

	if err := reporter.ReportOne(context.Background(), counter, wholeCounter(counter), NewReportClient(counter, srv.URL), time.Now()); err != nil {
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
	if err := reporter.ReportOne(context.Background(), counter, wholeCounter(counter), client, now); err != nil {
		t.Fatalf("ReportCounter: %v", err)
	}

	snapshot, err := reporter.db.GetSnapshot(context.Background(), counter.ID, "hour", "2026-09-12T14")
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

// ---- Scoped reports ----

// The point of scoping: a site-wide card averages away what is being watched.
func TestScopedReportSendsTheFilter(t *testing.T) {
	fake := &reportFake{
		byPeriod: map[string]string{"today": totals(100, 80, 300, 20, 3, 120, 15)},
		goals:    `{"goals":[]}`,
	}
	reporter, router, counter := newReporterHarness(t, fake)

	report := &model.Report{
		CounterID: counter.ID, Name: "Чекаут",
		URLFilter: "/checkout", URLMatch: URLMatchContains, Enabled: true,
	}
	client := NewReportClient(counter, fake.serve(t))
	if err := reporter.ReportOne(context.Background(), counter, report, client, time.Now()); err != nil {
		t.Fatalf("ReportOne: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	data := 0
	for _, q := range fake.queries {
		// The goal list is a settings lookup, not a data query; it carries no
		// filter by design.
		if q.Get("metrics") == "" {
			continue
		}
		data++
		if got := q.Get("filters"); got != `EXISTS(ym:pv:URL=@'/checkout')` {
			t.Errorf("a data request went out unscoped: metrics=%q filters=%q", q.Get("metrics"), got)
		}
	}
	if data == 0 {
		t.Fatal("no data request was made")
	}

	card := router.last()
	if !strings.Contains(card.title, "Чекаут") {
		t.Errorf("title does not name the report: %q", card.title)
	}
	// The card must say what it covers, or two reports look identical.
	if !strings.Contains(card.message, "/checkout") {
		t.Errorf("card does not state its scope:\n%s", card.message)
	}
}

// A report naming goals reports those goals, not every goal of the counter.
func TestReportCoversOnlyItsGoals(t *testing.T) {
	fake := &reportFake{
		byPeriod: map[string]string{"today": totals(100, 80, 300, 20, 3, 120, 15)},
		goals:    `{"goals":[{"id":42,"name":"Покупка"},{"id":77,"name":"Регистрация"},{"id":91,"name":"Подписка"}]}`,
	}
	reporter, router, counter := newReporterHarness(t, fake)

	report := &model.Report{
		CounterID: counter.ID, Name: "Заказы",
		GoalIDs: []int64{42, 91}, Enabled: true,
	}
	client := NewReportClient(counter, fake.serve(t))
	if err := reporter.ReportOne(context.Background(), counter, report, client, time.Now()); err != nil {
		t.Fatalf("ReportOne: %v", err)
	}

	var goalQuery string
	fake.mu.Lock()
	for _, q := range fake.queries {
		if strings.Contains(q.Get("metrics"), "reaches") && !strings.Contains(q.Get("metrics"), "ym:s:visits") {
			goalQuery = q.Get("metrics")
		}
	}
	fake.mu.Unlock()

	if goalQuery == "" {
		t.Fatal("no goal query was made")
	}
	for _, want := range []string{"ym:s:goal42reaches", "ym:s:goal91reaches"} {
		if !strings.Contains(goalQuery, want) {
			t.Errorf("goal query %q is missing %s", goalQuery, want)
		}
	}
	if strings.Contains(goalQuery, "goal77") {
		t.Errorf("goal query includes a goal the report does not cover: %q", goalQuery)
	}

	card := router.last().message
	if strings.Contains(card, "Регистрация") {
		t.Errorf("card reports a goal outside the report:\n%s", card)
	}
}

// Naming goal IDs must work even when the token cannot read counter settings —
// the IDs are already known, only the names are missing.
func TestReportWithGoalIDsSurvivesWithoutManagementAccess(t *testing.T) {
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

	report := &model.Report{CounterID: counter.ID, Name: "Заказы", GoalIDs: []int64{42}, Enabled: true}
	if err := reporter.ReportOne(context.Background(), counter, report, NewReportClient(counter, srv.URL), time.Now()); err != nil {
		t.Fatalf("ReportOne: %v", err)
	}

	card := router.last().message
	// Falls back to the goal's ID for its name rather than dropping it.
	if !strings.Contains(card, "цель 42") {
		t.Errorf("the selected goal was dropped when names were unavailable:\n%s", card)
	}
}

// An install that never configured a report keeps getting the whole-counter
// card it always got.
func TestUnconfiguredCounterStillReports(t *testing.T) {
	fake := &reportFake{
		byPeriod: map[string]string{"today": totals(1000, 800, 3000, 25, 3, 180, 40)},
		goals:    `{"goals":[]}`,
	}
	reporter, router, _ := newReporterHarness(t, fake)

	if err := reporter.RunReports(context.Background()); err != nil {
		t.Fatalf("RunReports: %v", err)
	}
	if router.count() != 1 {
		t.Fatalf("sent %d reports, want the implicit whole-counter one", router.count())
	}
	if !strings.Contains(router.last().message, "1000") {
		t.Errorf("card is empty:\n%s", router.last().message)
	}
}

// Once reports are configured they replace the implicit one, and each is sent.
func TestConfiguredReportsReplaceTheDefault(t *testing.T) {
	fake := &reportFake{
		byPeriod: map[string]string{"today": totals(100, 80, 300, 20, 3, 120, 15)},
		goals:    `{"goals":[]}`,
	}
	reporter, router, counter := newReporterHarness(t, fake)
	ctx := context.Background()

	for _, name := range []string{"Чекаут", "Каталог"} {
		if err := reporter.db.CreateReport(ctx, &model.Report{
			CounterID: counter.ID, Name: name,
			URLFilter: "/" + name, URLMatch: URLMatchContains, Enabled: true,
		}); err != nil {
			t.Fatalf("CreateReport: %v", err)
		}
	}

	if err := reporter.RunReports(ctx); err != nil {
		t.Fatalf("RunReports: %v", err)
	}
	if router.count() != 2 {
		t.Fatalf("sent %d reports, want one per configured report", router.count())
	}
}

// A disabled report is not sent, and disabling the last one falls back to the
// whole-counter card rather than going silent.
func TestDisabledReportIsSkipped(t *testing.T) {
	fake := &reportFake{
		byPeriod: map[string]string{"today": totals(100, 80, 300, 20, 3, 120, 15)},
		goals:    `{"goals":[]}`,
	}
	reporter, router, counter := newReporterHarness(t, fake)
	ctx := context.Background()

	if err := reporter.db.CreateReport(ctx, &model.Report{
		CounterID: counter.ID, Name: "Выключенный", Enabled: false,
	}); err != nil {
		t.Fatalf("CreateReport: %v", err)
	}

	if err := reporter.RunReports(ctx); err != nil {
		t.Fatalf("RunReports: %v", err)
	}
	if router.count() != 1 {
		t.Fatalf("sent %d reports", router.count())
	}
	if strings.Contains(router.last().title, "Выключенный") {
		t.Error("a disabled report was sent")
	}
}
