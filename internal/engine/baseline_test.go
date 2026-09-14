package engine

import (
	"math"
	"testing"
	"time"
)

// hourlySeries builds a series starting at `start` with one value per hour.
func hourlySeries(start time.Time, values []float64) *TimeSeries {
	intervals := make([]time.Time, len(values))
	for i := range values {
		intervals[i] = start.Add(time.Duration(i) * time.Hour)
	}
	return &TimeSeries{Intervals: intervals, Values: [][]float64{values}}
}

// weeklySeries builds four weeks of hourly data where every hour of every day
// carries `base`, then applies overrides at specific times.
func weeklySeries(t *testing.T, end time.Time, weeks int, base float64, overrides map[time.Time]float64) *TimeSeries {
	t.Helper()
	start := end.AddDate(0, 0, -7*weeks)
	hours := int(end.Sub(start).Hours()) + 1

	intervals := make([]time.Time, hours)
	values := make([]float64, hours)
	for i := range intervals {
		intervals[i] = start.Add(time.Duration(i) * time.Hour)
		values[i] = base
		if v, ok := overrides[intervals[i]]; ok {
			values[i] = v
		}
	}
	return &TimeSeries{Intervals: intervals, Values: [][]float64{values}}
}

func TestResolveMetric(t *testing.T) {
	tests := []struct {
		name string
		want string
		ok   bool
	}{
		{"visits", MetricVisits, true},
		{"VISITS", MetricVisits, true},
		{" users ", MetricUsers, true},
		{"pageviews", MetricPageviews, true},
		{"goals", MetricGoalReaches, true},
		{"goal:42", "ym:s:goal42reaches", true},
		{"goal: 42", "ym:s:goal42reaches", true},
		{"goal:abc", "", false},
		{"goal:", "", false},
		{"goal:0", "", false},
		{"нетакой", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		got, err := ResolveMetric(tc.name)
		if tc.ok && (err != nil || got != tc.want) {
			t.Errorf("ResolveMetric(%q) = (%q, %v), want %q", tc.name, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Errorf("ResolveMetric(%q) accepted an invalid metric", tc.name)
		}
	}
}

// The core of the design: an hour is compared with the same hour on the same
// weekday, never with the hours around it.
func TestBaselineUsesSameHourSameWeekday(t *testing.T) {
	// Tuesday 15:00.
	now := time.Date(2026, 9, 8, 15, 0, 0, 0, time.Local)
	if now.Weekday() != time.Tuesday {
		t.Fatalf("fixture is %s, expected Tuesday", now.Weekday())
	}

	overrides := map[time.Time]float64{
		now.AddDate(0, 0, -7):  100, // last Tuesday 15:00
		now.AddDate(0, 0, -14): 120, // two Tuesdays ago
		now.AddDate(0, 0, -21): 110, // three Tuesdays ago
		// Noise that must NOT enter the baseline:
		now.Add(-time.Hour):                  9000, // same day, an hour earlier
		now.AddDate(0, 0, -1):                9000, // Monday 15:00
		now.AddDate(0, 0, -7).Add(time.Hour): 9000, // last Tuesday 16:00
	}
	series := weeklySeries(t, now, 4, 50, overrides)

	baseline := BuildBaseline(series, 0, now)

	// Four Tuesdays at 15:00 sit in the window: 100, 120, 110 and the untouched
	// 50 from four weeks back. Median of those is 105.
	if baseline.Samples != 4 {
		t.Fatalf("samples = %d, want the 4 earlier Tuesdays at 15:00", baseline.Samples)
	}
	if math.Abs(baseline.Value-105) > 0.01 {
		t.Errorf("baseline = %v, want 105 — a neighbouring hour or weekday leaked in", baseline.Value)
	}
}

// The hour being judged must not be part of the baseline it is judged against,
// or a drop would drag its own expectation down with it.
func TestBaselineExcludesTheHourUnderTest(t *testing.T) {
	now := time.Date(2026, 9, 8, 15, 0, 0, 0, time.Local)
	series := weeklySeries(t, now, 2, 100, map[time.Time]float64{now: 0})

	baseline := BuildBaseline(series, 0, now)

	if baseline.Value != 100 {
		t.Errorf("baseline = %v, want 100 — the collapsed hour polluted its own baseline", baseline.Value)
	}
}

// One outlier week must not move the expectation much; that is why the median
// is used instead of the mean.
func TestBaselineMedianResistsOutliers(t *testing.T) {
	now := time.Date(2026, 9, 8, 15, 0, 0, 0, time.Local)
	overrides := map[time.Time]float64{
		now.AddDate(0, 0, -7):  100,
		now.AddDate(0, 0, -14): 100,
		now.AddDate(0, 0, -21): 100,
		now.AddDate(0, 0, -28): 10000, // a newsletter blast four weeks ago
	}
	series := weeklySeries(t, now, 5, 100, overrides)

	baseline := BuildBaseline(series, 0, now)

	if baseline.Value > 200 {
		t.Errorf("baseline = %v — one spike dragged the expectation up, a real drop would now go unnoticed", baseline.Value)
	}
}

func TestMedian(t *testing.T) {
	tests := []struct {
		in   []float64
		want float64
	}{
		{nil, 0},
		{[]float64{5}, 5},
		{[]float64{1, 3}, 2},
		{[]float64{3, 1, 2}, 2},
		{[]float64{4, 1, 3, 2}, 2.5},
		{[]float64{10, 10, 10, 1000}, 10},
	}
	for _, tc := range tests {
		if got := median(tc.in); math.Abs(got-tc.want) > 0.001 {
			t.Errorf("median(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestJudgeDirections(t *testing.T) {
	baseline := Baseline{Value: 100, Samples: 4}

	tests := []struct {
		name      string
		current   float64
		direction string
		threshold int
		want      bool
	}{
		{"drop past threshold", 50, DirectionDrop, 40, true},
		{"drop short of threshold", 80, DirectionDrop, 40, false},
		{"rise ignored by a drop rule", 200, DirectionDrop, 40, false},
		{"rise past threshold", 150, DirectionRise, 40, true},
		{"drop ignored by a rise rule", 10, DirectionRise, 40, false},
		{"both catches a drop", 50, DirectionBoth, 40, true},
		{"both catches a rise", 150, DirectionBoth, 40, true},
		{"both ignores normal variation", 110, DirectionBoth, 40, false},
		{"exactly at the threshold fires", 60, DirectionDrop, 40, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := Judge(tc.current, baseline, tc.direction, tc.threshold, 0, 2)
			if v.Fired != tc.want {
				t.Errorf("Judge(%v) fired = %v, want %v (deviation %+.0f%%, reason %q)",
					tc.current, v.Fired, tc.want, v.DeviationPct, v.Reason)
			}
		})
	}
}

// Without a noise floor, a slot that normally sees two visits alerts on every
// ordinary hour: one visit fewer is a 50% drop.
func TestJudgeNoiseFloor(t *testing.T) {
	quiet := Baseline{Value: 2, Samples: 4}

	if v := Judge(1, quiet, DirectionDrop, 40, 10, 2); v.Fired {
		t.Error("a two-visit hour fired an alert despite the noise floor")
	}
	if v := Judge(1, quiet, DirectionDrop, 40, 0, 2); !v.Fired {
		t.Error("with the floor disabled the same drop should fire")
	}
}

// A counter added days ago has no weekly history yet; alerting on a baseline
// of one sample would be guesswork.
func TestJudgeRequiresEnoughHistory(t *testing.T) {
	thin := Baseline{Value: 100, Samples: 1}

	v := Judge(10, thin, DirectionDrop, 40, 0, 2)
	if v.Fired {
		t.Error("fired on a single historical sample")
	}
	if v.Reason == "" {
		t.Error("a suppressed verdict should explain itself for the log")
	}
}

func TestJudgeReportsSignedDeviation(t *testing.T) {
	baseline := Baseline{Value: 100, Samples: 4}

	if v := Judge(40, baseline, DirectionBoth, 30, 0, 2); math.Abs(v.DeviationPct+60) > 0.01 {
		t.Errorf("drop deviation = %v, want -60", v.DeviationPct)
	}
	if v := Judge(160, baseline, DirectionBoth, 30, 0, 2); math.Abs(v.DeviationPct-60) > 0.01 {
		t.Errorf("rise deviation = %v, want +60", v.DeviationPct)
	}
}

func TestValidDirection(t *testing.T) {
	for _, d := range []string{DirectionDrop, DirectionRise, DirectionBoth} {
		if !ValidDirection(d) {
			t.Errorf("%q should be valid", d)
		}
	}
	for _, d := range []string{"", "up", "DROP", "падение"} {
		if ValidDirection(d) {
			t.Errorf("%q should be rejected", d)
		}
	}
}

func TestTimeSeriesLookup(t *testing.T) {
	start := time.Date(2026, 9, 8, 0, 0, 0, 0, time.Local)
	s := hourlySeries(start, []float64{10, 20, 30})

	if i := s.IndexOf(start.Add(2 * time.Hour)); i != 2 {
		t.Errorf("IndexOf = %d, want 2", i)
	}
	if i := s.IndexOf(start.Add(99 * time.Hour)); i != -1 {
		t.Errorf("IndexOf for an absent hour = %d, want -1", i)
	}
	if v, ok := s.At(0, 1); !ok || v != 20 {
		t.Errorf("At(0,1) = %v, %v", v, ok)
	}
	// Out-of-range reads must not panic on a short response.
	if _, ok := s.At(5, 0); ok {
		t.Error("At accepted a metric index past the end")
	}
	if _, ok := s.At(0, 99); ok {
		t.Error("At accepted an interval index past the end")
	}
}

func TestMetricLabel(t *testing.T) {
	tests := map[string]string{
		"visits":   "Визиты",
		"users":    "Посетители",
		"goals":    "Достижения целей",
		"goal:42":  "Цель 42",
		"неведомо": "неведомо",
	}
	for in, want := range tests {
		if got := MetricLabel(in); got != want {
			t.Errorf("MetricLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// Alert titles are read by people, so the verb has to agree with the metric's
// own label: "Визиты упали", but "Цель 42 упала".
func TestChangeVerbAgreesWithMetric(t *testing.T) {
	tests := []struct {
		metric       string
		deviationPct float64
		want         string
	}{
		{"visits", -70, "упали"},
		{"visits", 70, "выросли"},
		{"users", -30, "упали"},
		{"goals", -30, "упали"},
		{"goal:42", -91, "упала"},
		{"goal:42", 91, "выросла"},
		{"GOAL:7", -50, "упала"},
	}
	for _, tc := range tests {
		if got := changeVerb(tc.metric, tc.deviationPct); got != tc.want {
			t.Errorf("changeVerb(%q, %v) = %q, want %q", tc.metric, tc.deviationPct, got, tc.want)
		}
	}
}
