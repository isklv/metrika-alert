package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Metric names as they are written in triggers. They stay short and stable so
// a user types "visits", not "ym:s:visits", and a goal is addressed by its own
// Metrika ID.
const (
	MetricNameVisits    = "visits"
	MetricNameUsers     = "users"
	MetricNamePageviews = "pageviews"
	MetricNameGoals     = "goals"
)

// goalPrefix addresses one conversion goal, as in "goal:42".
const goalPrefix = "goal:"

// ResolveMetric translates a trigger's metric name into an API metric.
func ResolveMetric(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))

	switch name {
	case MetricNameVisits:
		return MetricVisits, nil
	case MetricNameUsers:
		return MetricUsers, nil
	case MetricNamePageviews:
		return MetricPageviews, nil
	case MetricNameGoals:
		return MetricGoalReaches, nil
	}

	if id, ok := strings.CutPrefix(name, goalPrefix); ok {
		goalID, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
		if err != nil || goalID <= 0 {
			return "", fmt.Errorf("метрика %q: после goal: нужен числовой ID цели", name)
		}
		return GoalReachesMetric(goalID), nil
	}

	return "", fmt.Errorf("неизвестная метрика %q: доступны visits, users, pageviews, goals, goal:<id>", name)
}

// MetricLabel renders a metric name for a human.
func MetricLabel(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case MetricNameVisits:
		return "Визиты"
	case MetricNameUsers:
		return "Посетители"
	case MetricNamePageviews:
		return "Просмотры"
	case MetricNameGoals:
		return "Достижения целей"
	}
	if id, ok := strings.CutPrefix(strings.ToLower(name), goalPrefix); ok {
		return "Цель " + strings.TrimSpace(id)
	}
	return name
}

// Directions a trigger can watch.
const (
	DirectionDrop = "drop"
	DirectionRise = "rise"
	DirectionBoth = "both"
)

// ValidDirection reports whether d is a supported direction.
func ValidDirection(d string) bool {
	switch d {
	case DirectionDrop, DirectionRise, DirectionBoth:
		return true
	}
	return false
}

// slot identifies a position in the weekly rhythm. Traffic at Tuesday 15:20 is
// only comparable with other Tuesdays at 15:20, so a slot is a weekday plus a
// minute of the day — not merely an hour, because the measurement window slides
// in ten-minute steps.
type slot struct {
	weekday     time.Weekday
	minuteOfDay int
}

func slotOf(t time.Time) slot {
	return slot{weekday: t.Weekday(), minuteOfDay: t.Hour()*60 + t.Minute()}
}

// Baseline is what one window is judged against.
type Baseline struct {
	// Value is the expected figure for this slot.
	Value float64
	// Samples is how many past occurrences of the slot it was built from.
	Samples int
}

// WindowSum totals a metric over [end-window, end).
//
// The measurement window is an hour wide even though it advances every ten
// minutes: a bare ten-minute bucket carries a sixth of the traffic and swings
// far too much week to week for a percentage threshold to mean anything.
// Summing six of them keeps the signal at hourly scale while the window still
// moves at ten-minute cadence.
func WindowSum(series *TimeSeries, metric int, end time.Time, window time.Duration) (float64, bool) {
	step := series.Step()
	if step <= 0 {
		return 0, false
	}

	var total float64
	for at := end.Add(-window); at.Before(end); at = at.Add(step) {
		i := series.IndexOf(at)
		if i < 0 {
			return 0, false
		}
		v, ok := series.At(metric, i)
		if !ok {
			return 0, false
		}
		total += v
	}
	return total, true
}

// BuildBaseline computes the expected value for the window ending at `end`,
// from the same weekday and time of day in earlier weeks.
//
// The median is used rather than the mean: one holiday, outage or newsletter
// spike in the history would drag a mean far enough to hide the very anomaly
// the trigger exists to catch.
func BuildBaseline(series *TimeSeries, metric int, end time.Time, window time.Duration, weeks int) Baseline {
	var samples []float64

	for week := 1; week <= weeks; week++ {
		past := end.AddDate(0, 0, -7*week)
		// Guard the weekly rhythm against a DST shift moving the slot.
		if slotOf(past) != slotOf(end) {
			continue
		}
		if v, ok := WindowSum(series, metric, past, window); ok {
			samples = append(samples, v)
		}
	}

	return Baseline{Value: median(samples), Samples: len(samples)}
}

// median returns the middle value of a sample set, or 0 when it is empty.
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)

	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// Verdict is the outcome of judging one hour against its baseline.
type Verdict struct {
	Current  float64
	Baseline Baseline
	// DeviationPct is signed: negative for a drop, positive for a rise.
	DeviationPct float64
	// Fired reports whether the trigger's condition was met.
	Fired bool
	// Reason explains a verdict that did not fire, for the log.
	Reason string
}

// Judge compares one hour's figure with its baseline.
//
// minBaseline is a noise floor. Without it a slot that normally sees two visits
// would alert on every ordinary hour, because one visit fewer is a 50% drop.
func Judge(current float64, baseline Baseline, direction string, deviationPct, minBaseline, minSamples int) Verdict {
	v := Verdict{Current: current, Baseline: baseline}

	if baseline.Samples < minSamples {
		v.Reason = fmt.Sprintf("истории мало: %d из %d нужных точек", baseline.Samples, minSamples)
		return v
	}
	if baseline.Value < float64(minBaseline) {
		v.Reason = fmt.Sprintf("база %.0f ниже порога значимости %d", baseline.Value, minBaseline)
		return v
	}

	v.DeviationPct = (current - baseline.Value) / baseline.Value * 100

	switch direction {
	case DirectionDrop:
		v.Fired = v.DeviationPct <= -float64(deviationPct)
	case DirectionRise:
		v.Fired = v.DeviationPct >= float64(deviationPct)
	case DirectionBoth:
		v.Fired = v.DeviationPct <= -float64(deviationPct) || v.DeviationPct >= float64(deviationPct)
	}

	if !v.Fired {
		v.Reason = fmt.Sprintf("отклонение %+.0f%% не достигло порога %d%%", v.DeviationPct, deviationPct)
	}
	return v
}

// weekdayNames renders a weekday in Russian, for alert text.
var weekdayNames = [...]string{"воскресенье", "понедельник", "вторник", "среда", "четверг", "пятница", "суббота"}

func weekdayName(d time.Weekday) string {
	if int(d) < len(weekdayNames) {
		return weekdayNames[d]
	}
	return d.String()
}
