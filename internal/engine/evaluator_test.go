package engine

import (
	"context"
	"encoding/json"
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

// byTimeFake serves /stat/v1/data/bytime from a generated hourly history.
type byTimeFake struct {
	mu sync.Mutex
	// value returns the figure for one metric at one hour.
	value    func(metric string, at time.Time) float64
	requests []url.Values
	sampled  bool
}

func (f *byTimeFake) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		q := r.URL.Query()
		f.requests = append(f.requests, q)

		if strings.HasSuffix(r.URL.Path, "/goals") {
			fmt.Fprint(w, `{"goals":[]}`)
			return
		}

		date1, _ := time.ParseInLocation("2006-01-02", q.Get("date1"), time.Local)
		date2, _ := time.ParseInLocation("2006-01-02", q.Get("date2"), time.Local)
		// The API reports whole days, so the range ends at the last hour of date2.
		end := date2.Add(23 * time.Hour)

		metrics := strings.Split(q.Get("metrics"), ",")
		var intervals [][]string
		var totals [][]float64
		for range metrics {
			totals = append(totals, nil)
		}
		for h := date1; !h.After(end); h = h.Add(time.Hour) {
			intervals = append(intervals, []string{h.Format("2006-01-02 15:04:05")})
			for i, m := range metrics {
				totals[i] = append(totals[i], f.value(m, h))
			}
		}

		json.NewEncoder(w).Encode(map[string]any{
			"totals":         totals,
			"time_intervals": intervals,
			"sampled":        f.sampled,
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (f *byTimeFake) lastRequest() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1]
}

type evalHarness struct {
	db        *model.DB
	evaluator *Evaluator
	router    *fakeRouter
	counter   *model.Counter
	fake      *byTimeFake
}

func newEvalHarness(t *testing.T, fake *byTimeFake) *evalHarness {
	t.Helper()
	db, err := model.OpenDB(filepath.Join(t.TempDir(), "eval.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	counter := testCounter()
	if err := db.CreateCounter(context.Background(), counter); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}

	router := &fakeRouter{}
	cfg := &MetrikaConfig{BaseURL: fake.serve(t)}
	return &evalHarness{db: db, evaluator: NewEvaluator(db, router, cfg), router: router, counter: counter, fake: fake}
}

func (h *evalHarness) addTrigger(t *testing.T, trigger *model.Trigger) *model.Trigger {
	t.Helper()
	trigger.CounterID = h.counter.ID
	if trigger.BaselineWeeks == 0 {
		trigger.BaselineWeeks = 4
	}
	trigger.Enabled = true
	if err := h.db.CreateTrigger(context.Background(), trigger); err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}
	return trigger
}

// tuesday3pm is the hour under test in these cases.
var tuesday3pm = time.Date(2026, 9, 8, 15, 0, 0, 0, time.Local)

// steadyExcept returns a value function that reports `normal` everywhere except
// at the given hours.
func steadyExcept(normal float64, overrides map[time.Time]float64) func(string, time.Time) float64 {
	return func(_ string, at time.Time) float64 {
		if v, ok := overrides[at]; ok {
			return v
		}
		return normal
	}
}

func TestVisitsDropFiresAlert(t *testing.T) {
	fake := &byTimeFake{value: steadyExcept(100, map[time.Time]float64{tuesday3pm: 30})}
	h := newEvalHarness(t, fake)
	h.addTrigger(t, &model.Trigger{
		Name: "Визиты упали", Metric: "visits", Direction: DirectionDrop,
		DeviationPct: 40, MinBaseline: 10, Cooldown: 180,
	})

	fired, err := h.evaluator.EvaluateHour(context.Background(), h.counter, tuesday3pm)
	if err != nil {
		t.Fatalf("EvaluateHour: %v", err)
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1", fired)
	}

	alert := h.router.last()
	if !strings.Contains(alert.title, "Визиты") || !strings.Contains(alert.title, "упали") {
		t.Errorf("title = %q", alert.title)
	}
	// 30 against a baseline of 100 is a 70% drop.
	if !strings.Contains(alert.title, "70%") {
		t.Errorf("title should carry the size of the drop: %q", alert.title)
	}
	for _, want := range []string{"вторник", "Обычно в этот час", "100", "30"} {
		if !strings.Contains(alert.message, want) {
			t.Errorf("message is missing %q:\n%s", want, alert.message)
		}
	}
}

// A normal evening lull must not alert: it is normal for that hour.
func TestQuietHourIsNotAnAnomaly(t *testing.T) {
	// 03:00 always sees 5 visits; 15:00 always sees 500.
	value := func(_ string, at time.Time) float64 {
		if at.Hour() == 3 {
			return 5
		}
		return 500
	}
	fake := &byTimeFake{value: value}
	h := newEvalHarness(t, fake)
	h.addTrigger(t, &model.Trigger{
		Name: "Визиты", Metric: "visits", Direction: DirectionDrop,
		DeviationPct: 40, MinBaseline: 3, Cooldown: 180,
	})

	night := time.Date(2026, 9, 8, 3, 0, 0, 0, time.Local)
	fired, err := h.evaluator.EvaluateHour(context.Background(), h.counter, night)
	if err != nil {
		t.Fatalf("EvaluateHour: %v", err)
	}
	if fired != 0 {
		t.Fatalf("the nightly lull fired %d alert(s) — a flat threshold mistake", fired)
	}
}

func TestRiseFiresOnlyForRiseRules(t *testing.T) {
	fake := &byTimeFake{value: steadyExcept(100, map[time.Time]float64{tuesday3pm: 300})}
	h := newEvalHarness(t, fake)
	h.addTrigger(t, &model.Trigger{
		Name: "Падение", Metric: "visits", Direction: DirectionDrop,
		DeviationPct: 40, MinBaseline: 10, Cooldown: 180,
	})

	fired, _ := h.evaluator.EvaluateHour(context.Background(), h.counter, tuesday3pm)
	if fired != 0 {
		t.Fatalf("a drop rule fired on a spike")
	}

	h.addTrigger(t, &model.Trigger{
		Name: "Всплеск", Metric: "visits", Direction: DirectionRise,
		DeviationPct: 40, MinBaseline: 10, Cooldown: 180,
	})
	fired, _ = h.evaluator.EvaluateHour(context.Background(), h.counter, tuesday3pm)
	if fired != 1 {
		t.Fatalf("fired = %d, want the rise rule to fire", fired)
	}
	if !strings.Contains(h.router.last().title, "выросли") {
		t.Errorf("title = %q", h.router.last().title)
	}
}

// Goals are the other thing worth alerting on: orders stopping is an incident
// even while traffic looks fine.
func TestGoalDropFiresAlert(t *testing.T) {
	value := func(metric string, at time.Time) float64 {
		if metric != "ym:s:goal42reaches" {
			return 1000 // traffic is healthy
		}
		if at.Equal(tuesday3pm) {
			return 1
		}
		return 20
	}
	fake := &byTimeFake{value: value}
	h := newEvalHarness(t, fake)
	h.addTrigger(t, &model.Trigger{
		Name: "Заказы просели", Metric: "goal:42", Direction: DirectionDrop,
		DeviationPct: 50, MinBaseline: 5, Cooldown: 180,
	})

	fired, err := h.evaluator.EvaluateHour(context.Background(), h.counter, tuesday3pm)
	if err != nil {
		t.Fatalf("EvaluateHour: %v", err)
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1", fired)
	}
	if !strings.Contains(h.router.last().title, "Цель 42") {
		t.Errorf("title = %q", h.router.last().title)
	}
}

// Several rules on one counter must cost one API call, not one each.
func TestTriggersShareOneRequest(t *testing.T) {
	fake := &byTimeFake{value: steadyExcept(100, nil)}
	h := newEvalHarness(t, fake)
	for i := range 5 {
		h.addTrigger(t, &model.Trigger{
			Name: fmt.Sprintf("Правило %d", i), Metric: "visits", Direction: DirectionDrop,
			DeviationPct: 40, MinBaseline: 10, Cooldown: 180,
		})
	}

	if _, err := h.evaluator.EvaluateHour(context.Background(), h.counter, tuesday3pm); err != nil {
		t.Fatalf("EvaluateHour: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 1 {
		t.Errorf("made %d requests for 5 rules on one metric, want 1", len(fake.requests))
	}
}

// Distinct metrics travel in one request too, up to the API's limit.
func TestDistinctMetricsShareOneRequest(t *testing.T) {
	fake := &byTimeFake{value: steadyExcept(100, nil)}
	h := newEvalHarness(t, fake)
	for _, metric := range []string{"visits", "users", "goal:42"} {
		h.addTrigger(t, &model.Trigger{
			Name: metric, Metric: metric, Direction: DirectionDrop,
			DeviationPct: 40, MinBaseline: 10, Cooldown: 180,
		})
	}

	if _, err := h.evaluator.EvaluateHour(context.Background(), h.counter, tuesday3pm); err != nil {
		t.Fatalf("EvaluateHour: %v", err)
	}

	got := fake.lastRequest().Get("metrics")
	for _, want := range []string{"ym:s:visits", "ym:s:users", "ym:s:goal42reaches"} {
		if !strings.Contains(got, want) {
			t.Errorf("metrics %q is missing %s", got, want)
		}
	}
	if len(fake.requests) != 1 {
		t.Errorf("made %d requests, want 1", len(fake.requests))
	}
}

// The requested window must span the deepest baseline any rule asks for.
func TestRequestWindowCoversTheDeepestBaseline(t *testing.T) {
	fake := &byTimeFake{value: steadyExcept(100, nil)}
	h := newEvalHarness(t, fake)
	h.addTrigger(t, &model.Trigger{
		Name: "Короткая", Metric: "visits", Direction: DirectionDrop,
		DeviationPct: 40, MinBaseline: 10, BaselineWeeks: 2, Cooldown: 180,
	})
	h.addTrigger(t, &model.Trigger{
		Name: "Длинная", Metric: "visits", Direction: DirectionDrop,
		DeviationPct: 40, MinBaseline: 10, BaselineWeeks: 8, Cooldown: 180,
	})

	if _, err := h.evaluator.EvaluateHour(context.Background(), h.counter, tuesday3pm); err != nil {
		t.Fatalf("EvaluateHour: %v", err)
	}

	date1, err := time.ParseInLocation("2006-01-02", fake.lastRequest().Get("date1"), time.Local)
	if err != nil {
		t.Fatalf("date1: %v", err)
	}
	if span := tuesday3pm.Sub(date1); span < 8*7*24*time.Hour {
		t.Errorf("window spans %s, too short for an 8-week baseline", span)
	}
	if fake.lastRequest().Get("group") != GroupHour {
		t.Errorf("group = %q, want hour", fake.lastRequest().Get("group"))
	}
}

func TestCooldownSuppressesRepeatAlerts(t *testing.T) {
	fake := &byTimeFake{value: steadyExcept(100, map[time.Time]float64{
		tuesday3pm:                20,
		tuesday3pm.Add(time.Hour): 20,
	})}
	h := newEvalHarness(t, fake)
	h.addTrigger(t, &model.Trigger{
		Name: "Визиты", Metric: "visits", Direction: DirectionDrop,
		DeviationPct: 40, MinBaseline: 10, Cooldown: 180,
	})

	if fired, _ := h.evaluator.EvaluateHour(context.Background(), h.counter, tuesday3pm); fired != 1 {
		t.Fatalf("first hour fired %d, want 1", fired)
	}
	if fired, _ := h.evaluator.EvaluateHour(context.Background(), h.counter, tuesday3pm.Add(time.Hour)); fired != 0 {
		t.Errorf("the next hour alerted again despite a 180-minute cooldown")
	}
}

func TestDisabledTriggerIsIgnored(t *testing.T) {
	fake := &byTimeFake{value: steadyExcept(100, map[time.Time]float64{tuesday3pm: 1})}
	h := newEvalHarness(t, fake)

	trigger := h.addTrigger(t, &model.Trigger{
		Name: "Визиты", Metric: "visits", Direction: DirectionDrop,
		DeviationPct: 40, MinBaseline: 10, Cooldown: 180,
	})
	if err := h.db.UpdateTrigger(context.Background(), trigger.ID, false); err != nil {
		t.Fatalf("UpdateTrigger: %v", err)
	}

	fired, err := h.evaluator.EvaluateHour(context.Background(), h.counter, tuesday3pm)
	if err != nil {
		t.Fatalf("EvaluateHour: %v", err)
	}
	if fired != 0 {
		t.Errorf("a disabled rule fired")
	}
	// A counter whose every rule is off should not call the API at all.
	if len(fake.requests) != 0 {
		t.Errorf("made %d requests with no enabled rules", len(fake.requests))
	}
}

func TestFiredAlertIsPersisted(t *testing.T) {
	fake := &byTimeFake{value: steadyExcept(100, map[time.Time]float64{tuesday3pm: 20})}
	h := newEvalHarness(t, fake)
	h.addTrigger(t, &model.Trigger{
		Name: "Визиты", Metric: "visits", Direction: DirectionDrop,
		DeviationPct: 40, MinBaseline: 10, Cooldown: 180,
	})

	if _, err := h.evaluator.EvaluateHour(context.Background(), h.counter, tuesday3pm); err != nil {
		t.Fatalf("EvaluateHour: %v", err)
	}

	alerts, err := h.db.RecentAlerts(context.Background(), h.counter.ID, 10)
	if err != nil {
		t.Fatalf("RecentAlerts: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("stored %d alerts, want 1", len(alerts))
	}
	// The absolute figure that fired it is kept for the history view.
	if alerts[0].EventCount != 20 {
		t.Errorf("event_count = %d, want the observed 20", alerts[0].EventCount)
	}
}

func TestSampledDataIsFlaggedInTheAlert(t *testing.T) {
	fake := &byTimeFake{
		value:   steadyExcept(100, map[time.Time]float64{tuesday3pm: 20}),
		sampled: true,
	}
	h := newEvalHarness(t, fake)
	h.addTrigger(t, &model.Trigger{
		Name: "Визиты", Metric: "visits", Direction: DirectionDrop,
		DeviationPct: 40, MinBaseline: 10, Cooldown: 180,
	})

	if _, err := h.evaluator.EvaluateHour(context.Background(), h.counter, tuesday3pm); err != nil {
		t.Fatalf("EvaluateHour: %v", err)
	}
	if !strings.Contains(h.router.last().message, "семплированы") {
		t.Errorf("a sampled figure was presented as exact:\n%s", h.router.last().message)
	}
}

// A rule naming a metric that no longer resolves must not take the whole
// counter's evaluation down with it.
func TestBadMetricDoesNotStopOtherRules(t *testing.T) {
	fake := &byTimeFake{value: steadyExcept(100, map[time.Time]float64{tuesday3pm: 20})}
	h := newEvalHarness(t, fake)

	h.addTrigger(t, &model.Trigger{
		Name: "Сломанная", Metric: "goal:abc", Direction: DirectionDrop,
		DeviationPct: 40, MinBaseline: 10, Cooldown: 180,
	})
	h.addTrigger(t, &model.Trigger{
		Name: "Рабочая", Metric: "visits", Direction: DirectionDrop,
		DeviationPct: 40, MinBaseline: 10, Cooldown: 180,
	})

	fired, err := h.evaluator.EvaluateHour(context.Background(), h.counter, tuesday3pm)
	if err != nil {
		t.Fatalf("EvaluateHour: %v", err)
	}
	if fired != 1 {
		t.Errorf("fired = %d, want the working rule to still fire", fired)
	}
}
