package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

func newPollerHarness(t *testing.T, fake *byTimeFake, cfg *MetrikaConfig) (*Poller, *model.DB, *model.Counter) {
	t.Helper()
	db, err := model.OpenDB(filepath.Join(t.TempDir(), "poll.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	counter := testCounter()
	if err := db.CreateCounter(context.Background(), counter); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}

	cfg.BaseURL = fake.serve(t)
	evaluator := NewEvaluator(db, &fakeRouter{}, cfg)
	return NewPoller(db, cfg, evaluator), db, counter
}

// Data is only judged once it has settled: Metrika keeps counting sessions for
// a while after they happen, so measuring up to the present reads low.
func TestPendingWindowsWaitsForDataToSettle(t *testing.T) {
	p := NewPoller(nil, &MetrikaConfig{SettleMinutes: 20, WindowMinutes: 60, StepMinutes: 10, MaxCatchUpSteps: 6}, nil)

	// 14:35 — with 20 minutes to settle, everything through 14:10 is countable.
	now := time.Date(2026, 9, 8, 14, 35, 0, 0, time.Local)
	windows := p.pendingWindows(&model.Counter{}, now)

	if len(windows) != 1 {
		t.Fatalf("got %d windows, want 1", len(windows))
	}
	if got := windows[0]; got.Hour() != 14 || got.Minute() != 10 {
		t.Errorf("window ends %s, want 14:10", got.Format("15:04"))
	}
}

// The whole point of the ten-minute step: a drop is noticed within minutes,
// not at the end of the hour.
func TestWindowAdvancesEveryStep(t *testing.T) {
	p := NewPoller(nil, &MetrikaConfig{SettleMinutes: 20, WindowMinutes: 60, StepMinutes: 10, MaxCatchUpSteps: 6}, nil)
	now := time.Date(2026, 9, 8, 14, 35, 0, 0, time.Local)

	previous := time.Date(2026, 9, 8, 14, 0, 0, 0, time.Local)
	windows := p.pendingWindows(&model.Counter{LastHourChecked: &previous}, now)

	if len(windows) != 1 {
		t.Fatalf("got %d windows, want 1", len(windows))
	}
	if !windows[0].Equal(previous.Add(10 * time.Minute)) {
		t.Errorf("next window ends %s, want one step on from %s",
			windows[0].Format("15:04"), previous.Format("15:04"))
	}

	// Ten minutes later there is exactly one more window to judge.
	windows = p.pendingWindows(&model.Counter{LastHourChecked: &previous}, now.Add(10*time.Minute))
	if len(windows) != 2 {
		t.Errorf("got %d windows after another step, want 2", len(windows))
	}
}

// Window ends land on step boundaries so a slot means the same thing week to
// week; an unaligned cursor must be pulled back onto the grid.
func TestWindowEndsAlignToTheStepGrid(t *testing.T) {
	p := NewPoller(nil, &MetrikaConfig{SettleMinutes: 20, WindowMinutes: 60, StepMinutes: 10, MaxCatchUpSteps: 6}, nil)
	now := time.Date(2026, 9, 8, 14, 37, 0, 0, time.Local)

	for _, w := range p.pendingWindows(&model.Counter{}, now) {
		if w.Minute()%10 != 0 || w.Second() != 0 {
			t.Errorf("window end %s is off the ten-minute grid", w.Format("15:04:05"))
		}
	}

	skewed := time.Date(2026, 9, 8, 13, 47, 0, 0, time.Local)
	for _, w := range p.pendingWindows(&model.Counter{LastHourChecked: &skewed}, now) {
		if w.Minute()%10 != 0 {
			t.Errorf("window end %s is off the grid after a skewed cursor", w.Format("15:04"))
		}
	}
}

// A counter that has never been checked starts at the latest window rather than
// replaying history — those windows are past and alerting on them helps nobody.
func TestFreshCounterDoesNotReplayHistory(t *testing.T) {
	p := NewPoller(nil, &MetrikaConfig{SettleMinutes: 20, WindowMinutes: 60, StepMinutes: 10, MaxCatchUpSteps: 6}, nil)
	now := time.Date(2026, 9, 8, 14, 35, 0, 0, time.Local)

	windows := p.pendingWindows(&model.Counter{LastHourChecked: nil}, now)

	if len(windows) != 1 {
		t.Errorf("a fresh counter queued %d windows, want 1", len(windows))
	}
}

// After downtime the missed windows are worked through, but only so many at once.
func TestCatchUpIsBounded(t *testing.T) {
	p := NewPoller(nil, &MetrikaConfig{SettleMinutes: 20, WindowMinutes: 60, StepMinutes: 10, MaxCatchUpSteps: 6}, nil)
	now := time.Date(2026, 9, 8, 14, 35, 0, 0, time.Local)

	lastChecked := now.AddDate(0, 0, -3)
	windows := p.pendingWindows(&model.Counter{LastHourChecked: &lastChecked}, now)

	if len(windows) != 6 {
		t.Fatalf("queued %d windows, want the 6-step cap", len(windows))
	}
	// Oldest first, so the cursor advances without gaps.
	for i := 1; i < len(windows); i++ {
		if !windows[i].Equal(windows[i-1].Add(10 * time.Minute)) {
			t.Fatalf("windows are not consecutive: %v", windows)
		}
	}
}

func TestNothingPendingWhenUpToDate(t *testing.T) {
	p := NewPoller(nil, &MetrikaConfig{SettleMinutes: 20, WindowMinutes: 60, StepMinutes: 10, MaxCatchUpSteps: 6}, nil)
	now := time.Date(2026, 9, 8, 14, 35, 0, 0, time.Local)

	current := truncateTo(now.Add(-p.cfg.settle()), p.cfg.step())
	if windows := p.pendingWindows(&model.Counter{LastHourChecked: &current}, now); len(windows) != 0 {
		t.Errorf("queued %d windows while already current", len(windows))
	}
}

// The cursor has to survive a restart, or the service re-judges hours it
// already alerted on.
func TestCursorAdvancesAndPersists(t *testing.T) {
	fake := &byTimeFake{value: steadyExcept(100, nil)}
	p, db, counter := newPollerHarness(t, fake, &MetrikaConfig{SettleMinutes: 20, MaxCatchUpSteps: 6})
	ctx := context.Background()

	if err := db.CreateTrigger(ctx, &model.Trigger{
		CounterID: counter.ID, Name: "Визиты", Metric: "visits", Direction: DirectionDrop,
		DeviationPct: 40, MinBaseline: 10, BaselineWeeks: 4, Cooldown: 180, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}

	if err := p.advance(ctx, counter.ID); err != nil {
		t.Fatalf("advance: %v", err)
	}

	updated, err := db.GetCounter(ctx, counter.ID)
	if err != nil {
		t.Fatalf("GetCounter: %v", err)
	}
	if updated.LastHourChecked == nil {
		t.Fatal("cursor was not advanced")
	}

	// A second pass has nothing left to do, so it makes no further API calls.
	before := len(fake.requests)
	if err := p.advance(ctx, counter.ID); err != nil {
		t.Fatalf("second advance: %v", err)
	}
	if len(fake.requests) != before {
		t.Errorf("re-judged an hour that was already done")
	}
}

// A failed hour must not advance the cursor, or it is silently skipped.
func TestFailedHourIsRetried(t *testing.T) {
	fake := &byTimeFake{value: steadyExcept(100, nil)}
	p, db, counter := newPollerHarness(t, fake, &MetrikaConfig{SettleMinutes: 20, MaxCatchUpSteps: 6})
	ctx := context.Background()

	// A rule whose metric cannot resolve makes the whole fetch fail.
	if err := db.CreateTrigger(ctx, &model.Trigger{
		CounterID: counter.ID, Name: "Сломанная", Metric: "goal:abc", Direction: DirectionDrop,
		DeviationPct: 40, MinBaseline: 10, BaselineWeeks: 4, Cooldown: 180, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}

	if err := p.advance(ctx, counter.ID); err == nil {
		t.Fatal("expected the failure to surface")
	}

	updated, _ := db.GetCounter(ctx, counter.ID)
	if updated.LastHourChecked != nil {
		t.Error("cursor advanced past an hour that was never judged")
	}
}

func TestMetrikaConfigDefaults(t *testing.T) {
	cfg := &MetrikaConfig{}
	if cfg.settle() != DefaultSettleMinutes*time.Minute {
		t.Errorf("settle = %s", cfg.settle())
	}
	if cfg.maxCatchUp() != DefaultMaxCatchUpSteps {
		t.Errorf("maxCatchUp = %d", cfg.maxCatchUp())
	}
	if cfg.window() != DefaultWindowMinutes*time.Minute {
		t.Errorf("window = %s", cfg.window())
	}
	if cfg.step() != DefaultStepMinutes*time.Minute {
		t.Errorf("step = %s", cfg.step())
	}
	// A window narrower than the step would leave gaps between measurements.
	if cfg.window() < cfg.step() {
		t.Error("the window must be at least as wide as the step")
	}
}

func TestRunOnceNeedsCounters(t *testing.T) {
	db, err := model.OpenDB(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	p := NewPoller(db, &MetrikaConfig{}, nil)
	if err := p.RunOnce(context.Background()); err == nil {
		t.Error("expected an error with no counters configured")
	}
}
