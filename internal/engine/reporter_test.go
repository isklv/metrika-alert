package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

func openReporterDB(t *testing.T) *model.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := model.OpenDB(filepath.Join(dir, "reporter.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestBuildSnapshot_Full(t *testing.T) {
	r := &Reporter{}
	data := []map[string]any{{
		"visits":           float64(100),
		"unique_visits":    float64(80),
		"bounce_rate":      float64(40),
		"avg_time_on_site": float64(120),
		"avg_depth":        float64(2.5),
	}}
	goals := []map[string]any{
		{"goal_id": float64(7), "reaches": float64(12)},
		{"goal_id": float64(9), "reaches": float64(3)},
	}
	s := r.buildSnapshot(1, nil, "hour", "2026-08-01T10", data, goals)

	if s.CounterID != 1 || s.Period != "hour" || s.PeriodKey != "2026-08-01T10" {
		t.Errorf("snapshot header = %+v", s)
	}
	if s.Visits != 100 || s.UniqueVisits != 80 {
		t.Errorf("visits/unique = %d/%d", s.Visits, s.UniqueVisits)
	}
	// Bounces = 40% of 100 = 40.
	if s.Bounces != 40 {
		t.Errorf("bounces = %d, want 40", s.Bounces)
	}
	if s.AvgDuration != 120 || s.Depth != 2.5 {
		t.Errorf("duration/depth = %v/%v", s.AvgDuration, s.Depth)
	}
	if len(s.Goals) != 2 || s.Goals["7"] != 12 || s.Goals["9"] != 3 {
		t.Errorf("goals = %v", s.Goals)
	}
	if s.MonitorID != nil {
		t.Errorf("monitor id = %v, want nil", *s.MonitorID)
	}
}

func TestBuildSnapshot_EmptyData(t *testing.T) {
	r := &Reporter{}
	s := r.buildSnapshot(2, nil, "hour", "key", nil, nil)
	if s.Visits != 0 || s.UniqueVisits != 0 || s.Bounces != 0 || len(s.Goals) != 0 {
		t.Errorf("expected zero snapshot, got %+v", s)
	}
	if s.Goals == nil {
		t.Error("Goals map should be initialized")
	}
}

func TestBuildSnapshot_MonitorID(t *testing.T) {
	r := &Reporter{}
	mid := int64(5)
	s := r.buildSnapshot(1, &mid, "hour", "key", nil, nil)
	if s.MonitorID == nil || *s.MonitorID != 5 {
		t.Errorf("monitor id = %v, want 5", s.MonitorID)
	}
}

func TestBuildCard_NoData(t *testing.T) {
	r := &Reporter{}
	card := r.buildCard("Shop", nil, nil, nil, nil, nil)
	if !strings.Contains(card, "Нет данных") {
		t.Errorf("card = %q, want no-data message", card)
	}
}

func TestBuildCard_Full(t *testing.T) {
	r := &Reporter{}
	current := []map[string]any{{
		"visits":           float64(100),
		"unique_visits":    float64(80),
		"bounce_rate":      float64(40),
		"avg_depth":        float64(2.5),
	}}
	yesterday := []map[string]any{{"visits": float64(50), "unique_visits": float64(40)}}
	lastWeek := []map[string]any{{"visits": float64(200), "unique_visits": float64(160)}}
	lastMonth := []map[string]any{{"visits": float64(100), "unique_visits": float64(80)}}
	goals := []map[string]any{{"goal_id": float64(7), "reaches": float64(12)}}

	card := r.buildCard("Shop", current, yesterday, lastWeek, lastMonth, goals)

	if !strings.Contains(card, "Отчёт: Shop") {
		t.Errorf("card missing title: %q", card)
	}
	if !strings.Contains(card, "Посещения") || !strings.Contains(card, "100") {
		t.Errorf("card missing visits line: %q", card)
	}
	if !strings.Contains(card, "Уникальные") {
		t.Errorf("card missing unique line: %q", card)
	}
	if !strings.Contains(card, "Отказы") || !strings.Contains(card, "40%") {
		t.Errorf("card missing bounces: %q", card)
	}
	if !strings.Contains(card, "Глубина") {
		t.Errorf("card missing depth: %q", card)
	}
	if !strings.Contains(card, "Цели") || !strings.Contains(card, "7") {
		t.Errorf("card missing goals: %q", card)
	}
	// visits 100 vs yesterday 50 => +100% (▲).
	if !strings.Contains(card, "▲100%") {
		t.Errorf("card missing +100%% up arrow: %q", card)
	}
	// visits 100 vs lastWeek 200 => -50% (▼).
	if !strings.Contains(card, "▼50%") {
		t.Errorf("card missing -50%% down arrow: %q", card)
	}
}

func TestMetricLine_ZeroCurrent(t *testing.T) {
	r := &Reporter{}
	// Current value missing/zero => empty line.
	if got := r.metricLine("X", float64(0), "visits", nil, nil, nil); got != "" {
		t.Errorf("metricLine zero = %q, want empty", got)
	}
	if got := r.metricLine("X", "not-a-number", "visits", nil, nil, nil); got != "" {
		t.Errorf("metricLine bad type = %q, want empty", got)
	}
}

func TestMetricLine_NoComparisons(t *testing.T) {
	r := &Reporter{}
	got := r.metricLine("Visits", float64(100), "visits", nil, nil, nil)
	if !strings.Contains(got, "100") || strings.Contains(got, "▲") || strings.Contains(got, "▼") {
		t.Errorf("metricLine = %q, want just the value", got)
	}
}

func TestMetricLine_MixedComparisons(t *testing.T) {
	r := &Reporter{}
	yesterday := []map[string]any{{"visits": float64(50)}}
	lastWeek := []map[string]any{} // empty, skipped
	lastMonth := []map[string]any{{"visits": float64(200)}}
	got := r.metricLine("Visits", float64(100), "visits", yesterday, lastWeek, lastMonth)
	if !strings.Contains(got, "▲100%") {
		t.Errorf("missing +100%%: %q", got)
	}
	if !strings.Contains(got, "▼50%") {
		t.Errorf("missing -50%%: %q", got)
	}
}

func TestReportCounter_SavesSnapshotAndAlerts(t *testing.T) {
	db := openReporterDB(t)
	ctx := context.Background()

	c := &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"visits":10,"unique_visits":8,"bounce_rate":20,"avg_depth":1.5,"avg_time_on_site":60}]}`))
	}))
	t.Cleanup(srv.Close)

	router := &alertRouter{}
	rep := NewReporter(db, &MetrikaConfig{BaseURL: srv.URL, LogsURL: srv.URL}, router)
	client := NewMetrikaClient(c, srv.URL, srv.URL)

	now := time.Date(2026, 8, 1, 10, 30, 0, 0, time.UTC)
	if err := rep.ReportCounter(ctx, c, client, now); err != nil {
		t.Fatalf("ReportCounter: %v", err)
	}

	// Alert delivered.
	if len(router.alerts) != 1 {
		t.Fatalf("alerts = %d, want 1", len(router.alerts))
	}
	if !strings.Contains(router.alerts[0].title, "Отчёт") {
		t.Errorf("alert title = %q", router.alerts[0].title)
	}
	if !strings.Contains(router.alerts[0].message, "Посещения") {
		t.Errorf("alert message missing visits: %q", router.alerts[0].message)
	}

	// Snapshot saved.
	snaps, err := db.ListSnapshots(ctx, c.ID, "hour", 10)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(snaps))
	}
	if snaps[0].Visits != 10 || snaps[0].UniqueVisits != 8 {
		t.Errorf("snapshot visits/unique = %d/%d", snaps[0].Visits, snaps[0].UniqueVisits)
	}
}

func TestReportCounter_FetchError(t *testing.T) {
	db := openReporterDB(t)
	ctx := context.Background()
	c := &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":401,"message":"bad token"}}`))
	}))
	t.Cleanup(srv.Close)

	router := &alertRouter{}
	rep := NewReporter(db, &MetrikaConfig{BaseURL: srv.URL, LogsURL: srv.URL}, router)
	client := NewMetrikaClient(c, srv.URL, srv.URL)

	err := rep.ReportCounter(ctx, c, client, time.Now())
	if err == nil {
		t.Fatal("expected error from current-period fetch failure")
	}
	if !strings.Contains(err.Error(), "fetch current report") {
		t.Errorf("error = %q, want fetch current report", err)
	}
	if len(router.alerts) != 0 {
		t.Errorf("alerts = %d, want 0 on fetch failure", len(router.alerts))
	}
}

func TestRunReports_NoCounters(t *testing.T) {
	db := openReporterDB(t)
	rep := NewReporter(db, &MetrikaConfig{BaseURL: "http://unused"}, &alertRouter{})
	if err := rep.RunReports(context.Background()); err != nil {
		t.Fatalf("RunReports (no counters): %v", err)
	}
}

func TestRunReports_WithCounters(t *testing.T) {
	db := openReporterDB(t)
	ctx := context.Background()

	c := &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"visits":10,"unique_visits":8,"bounce_rate":20,"avg_depth":1.5,"avg_time_on_site":60}]}`))
	}))
	t.Cleanup(srv.Close)

	router := &alertRouter{}
	rep := NewReporter(db, &MetrikaConfig{BaseURL: srv.URL, LogsURL: srv.URL}, router)
	if err := rep.RunReports(ctx); err != nil {
		t.Fatalf("RunReports: %v", err)
	}
	// One report delivered for the single counter.
	if len(router.alerts) != 1 {
		t.Fatalf("alerts = %d, want 1", len(router.alerts))
	}
	// Snapshot persisted for the counter.
	snaps, err := db.ListSnapshots(ctx, c.ID, "hour", 10)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(snaps))
	}
}
