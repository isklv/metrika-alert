package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

func newSchedulerHarness(t *testing.T, fallback Schedule) (*ReportScheduler, *model.DB, *fakeRouter) {
	t.Helper()
	db, err := model.OpenDB(filepath.Join(t.TempDir(), "sched.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	fake := &reportFake{byPeriod: map[string]string{"today": totals(100, 80, 300, 20, 3, 120, 15)}, goals: `{"goals":[]}`}
	cfg := &MetrikaConfig{BaseURL: fake.serve(t)}
	if err := db.CreateCounter(context.Background(), testCounter()); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}

	router := &fakeRouter{}
	return NewReportScheduler(db, NewReporter(db, cfg, router), fallback), db, router
}

func (f *fakeRouter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.alerts)
}

// Until a schedule is stored, config.yaml still governs — an install configured
// only through the file must keep working.
func TestSchedulerFallsBackToConfig(t *testing.T) {
	fallback := Schedule{Kind: ScheduleEvery, Every: 6 * time.Hour}
	s, _, _ := newSchedulerHarness(t, fallback)

	if got := s.Schedule(context.Background()); got != fallback {
		t.Errorf("schedule = %+v, want the configured fallback", got)
	}
}

// A schedule set from a chat overrides the file and survives a restart.
func TestStoredScheduleOverridesConfig(t *testing.T) {
	s, db, _ := newSchedulerHarness(t, Schedule{Kind: ScheduleEvery, Every: time.Hour})
	ctx := context.Background()

	daily := Schedule{Kind: ScheduleDaily, Hour: 10}
	if err := s.SetSchedule(ctx, daily); err != nil {
		t.Fatalf("SetSchedule: %v", err)
	}
	if got := s.Schedule(ctx); got != daily {
		t.Errorf("schedule = %+v, want %+v", got, daily)
	}

	// A fresh scheduler over the same database is what a restart looks like.
	restarted := NewReportScheduler(db, nil, Schedule{Kind: ScheduleEvery, Every: time.Hour})
	if got := restarted.Schedule(ctx); got != daily {
		t.Errorf("after restart schedule = %+v, want the stored %+v", got, daily)
	}
}

// A stored value that cannot be read must not wedge reports forever.
func TestUnreadableScheduleFallsBack(t *testing.T) {
	fallback := Schedule{Kind: ScheduleEvery, Every: 6 * time.Hour}
	s, db, _ := newSchedulerHarness(t, fallback)
	ctx := context.Background()

	if err := db.SetSetting(ctx, model.SettingReportSchedule, "по вторникам"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got := s.Schedule(ctx); got != fallback {
		t.Errorf("schedule = %+v, want the fallback", got)
	}
}

func TestSchedulerSendsWhenDue(t *testing.T) {
	s, db, router := newSchedulerHarness(t, Schedule{Kind: ScheduleOff})
	ctx := context.Background()

	if err := s.SetSchedule(ctx, Schedule{Kind: ScheduleDaily, Hour: 10}); err != nil {
		t.Fatalf("SetSchedule: %v", err)
	}

	// Yesterday's run recorded; today at 10:00 a report is due.
	if err := db.SetSettingTime(ctx, model.SettingReportLastRun,
		time.Date(2026, 9, 14, 10, 0, 0, 0, time.Local)); err != nil {
		t.Fatalf("SetSettingTime: %v", err)
	}

	s.tick(ctx, time.Date(2026, 9, 15, 10, 0, 0, 0, time.Local))
	if router.count() != 1 {
		t.Fatalf("sent %d reports, want 1", router.count())
	}

	// A second tick the same day must stay quiet.
	s.tick(ctx, time.Date(2026, 9, 15, 10, 1, 0, 0, time.Local))
	if router.count() != 1 {
		t.Errorf("sent %d reports, want the day's single one", router.count())
	}
}

func TestSchedulerStaysQuietWhenOff(t *testing.T) {
	s, _, router := newSchedulerHarness(t, Schedule{Kind: ScheduleOff})

	s.tick(context.Background(), time.Now())
	if router.count() != 0 {
		t.Errorf("a disabled schedule sent %d reports", router.count())
	}
}

// The run is recorded before the report is sent: a failure mid-report must not
// queue the same report again on every tick that follows.
func TestFailedReportDoesNotRepeatForever(t *testing.T) {
	db, err := model.OpenDB(filepath.Join(t.TempDir(), "fail.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// No counters, so RunReports fails.
	router := &fakeRouter{}
	s := NewReportScheduler(db, NewReporter(db, &MetrikaConfig{BaseURL: "http://127.0.0.1:1"}, router),
		Schedule{Kind: ScheduleEvery, Every: time.Hour})

	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.Local)
	s.tick(ctx, now)

	last, err := db.SettingTime(ctx, model.SettingReportLastRun)
	if err != nil {
		t.Fatalf("SettingTime: %v", err)
	}
	if last.IsZero() {
		t.Fatal("a failed report left no record, so it would retry on every tick")
	}

	// A minute later nothing more is attempted.
	s.tick(ctx, now.Add(time.Minute))
	if got, _ := db.SettingTime(ctx, model.SettingReportLastRun); !got.Equal(last) {
		t.Error("the failed report was retried immediately")
	}
}

func TestNextRunReportsWhenTheReportLands(t *testing.T) {
	s, db, _ := newSchedulerHarness(t, Schedule{Kind: ScheduleOff})
	ctx := context.Background()

	if err := s.SetSchedule(ctx, Schedule{Kind: ScheduleDaily, Hour: 10}); err != nil {
		t.Fatalf("SetSchedule: %v", err)
	}
	if err := db.SetSettingTime(ctx, model.SettingReportLastRun, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("SetSettingTime: %v", err)
	}

	next, ok := s.NextRun(ctx)
	if !ok {
		t.Fatal("no next run reported")
	}
	if next.Hour() != 10 || next.Minute() != 0 {
		t.Errorf("next run = %v, want 10:00", next)
	}
}
