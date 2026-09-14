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

// Only whole hours are judged: the hour in progress is still filling, and
// comparing it against complete ones would report a drop every time.
func TestPendingHoursSkipsTheHourInProgress(t *testing.T) {
	p := NewPoller(nil, &MetrikaConfig{SettleMinutes: 20, MaxCatchUpHours: 6}, nil)

	// 14:35 — the 14:00 hour has not closed yet.
	now := time.Date(2026, 9, 8, 14, 35, 0, 0, time.Local)
	hours := p.pendingHours(&model.Counter{}, now)

	if len(hours) != 1 {
		t.Fatalf("got %d hours, want 1", len(hours))
	}
	if hours[0].Hour() != 13 {
		t.Errorf("judged hour %d:00, want 13:00 — the hour in progress was included", hours[0].Hour())
	}
}

// Metrika keeps counting sessions past the boundary, so a freshly closed hour
// must be left to settle before it is judged.
func TestPendingHoursWaitsForTheHourToSettle(t *testing.T) {
	p := NewPoller(nil, &MetrikaConfig{SettleMinutes: 20, MaxCatchUpHours: 6}, nil)

	// 15:05 — the 14:00 hour closed five minutes ago, inside the settle window.
	now := time.Date(2026, 9, 8, 15, 5, 0, 0, time.Local)
	hours := p.pendingHours(&model.Counter{}, now)

	if hours[0].Hour() != 13 {
		t.Errorf("judged %d:00 only five minutes after it closed", hours[0].Hour())
	}

	// 15:25 — now 14:00 has settled.
	hours = p.pendingHours(&model.Counter{}, now.Add(20*time.Minute))
	if hours[0].Hour() != 14 {
		t.Errorf("judged %d:00, want 14:00 once the settle window passed", hours[0].Hour())
	}
}

// A counter that has never been checked starts at the previous hour rather than
// replaying history — those hours are long past and alerting on them helps nobody.
func TestFreshCounterDoesNotReplayHistory(t *testing.T) {
	p := NewPoller(nil, &MetrikaConfig{SettleMinutes: 20, MaxCatchUpHours: 6}, nil)
	now := time.Date(2026, 9, 8, 14, 35, 0, 0, time.Local)

	hours := p.pendingHours(&model.Counter{LastHourChecked: nil}, now)

	if len(hours) != 1 {
		t.Errorf("a fresh counter queued %d hours, want 1", len(hours))
	}
}

// After downtime the missed hours are worked through, but only so many at once.
func TestCatchUpIsBounded(t *testing.T) {
	p := NewPoller(nil, &MetrikaConfig{SettleMinutes: 20, MaxCatchUpHours: 6}, nil)
	now := time.Date(2026, 9, 8, 14, 35, 0, 0, time.Local)

	lastChecked := now.AddDate(0, 0, -3)
	hours := p.pendingHours(&model.Counter{LastHourChecked: &lastChecked}, now)

	if len(hours) != 6 {
		t.Fatalf("queued %d hours, want the 6-hour cap", len(hours))
	}
	// Oldest first, so the cursor advances without gaps.
	for i := 1; i < len(hours); i++ {
		if !hours[i].Equal(hours[i-1].Add(time.Hour)) {
			t.Fatalf("hours are not consecutive: %v", hours)
		}
	}
	if !hours[0].Equal(lastChecked.Truncate(time.Hour).Add(time.Hour)) {
		t.Errorf("catch-up starts at %s, want the hour after the cursor", hours[0])
	}
}

func TestNothingPendingWhenUpToDate(t *testing.T) {
	p := NewPoller(nil, &MetrikaConfig{SettleMinutes: 20, MaxCatchUpHours: 6}, nil)
	now := time.Date(2026, 9, 8, 14, 35, 0, 0, time.Local)

	current := now.Add(-p.cfg.settle()).Truncate(time.Hour)
	if hours := p.pendingHours(&model.Counter{LastHourChecked: &current}, now); len(hours) != 0 {
		t.Errorf("queued %d hours while already current", len(hours))
	}
}

// The cursor has to survive a restart, or the service re-judges hours it
// already alerted on.
func TestCursorAdvancesAndPersists(t *testing.T) {
	fake := &byTimeFake{value: steadyExcept(100, nil)}
	p, db, counter := newPollerHarness(t, fake, &MetrikaConfig{SettleMinutes: 20, MaxCatchUpHours: 6})
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
	p, db, counter := newPollerHarness(t, fake, &MetrikaConfig{SettleMinutes: 20, MaxCatchUpHours: 6})
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
	if cfg.maxCatchUp() != DefaultMaxCatchUpHours {
		t.Errorf("maxCatchUp = %d", cfg.maxCatchUp())
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
