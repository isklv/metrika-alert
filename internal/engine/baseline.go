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

// slot identifies a position in the weekly rhythm: traffic at Tuesday 15:00 is
// only comparable with other Tuesdays at 15:00.
type slot struct {
	weekday time.Weekday
	hour    int
}

func slotOf(t time.Time) slot {
	return slot{weekday: t.Weekday(), hour: t.Hour()}
}

// Baseline is what one hour is judged against.
type Baseline struct {
	// Value is the expected figure for this slot.
	Value float64
	// Samples is how many past occurrences of the slot it was built from.
	Samples int
}

// BuildBaseline computes the expected value for the slot at `at`, from the same
// weekday and hour in earlier weeks of the series.
//
// The median is used rather than the mean: one holiday, outage or newsletter
// spike in the history would drag a mean far enough to hide the very anomaly
// the trigger exists to catch.
func BuildBaseline(series *TimeSeries, metric int, at time.Time) Baseline {
	target := slotOf(at)

	var samples []float64
	for i, interval := range series.Intervals {
		// The hour being judged is not part of its own baseline, and neither is
		// anything after it.
		if !interval.Before(at) {
			continue
		}
		if slotOf(interval) != target {
			continue
		}
		if v, ok := series.At(metric, i); ok {
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
