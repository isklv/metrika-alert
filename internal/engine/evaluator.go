package engine

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

// AlertRouter delivers a fired alert.
type AlertRouter interface {
	Alert(ctx context.Context, counterID int64, title, message string) error
}

// Evaluator judges one counter's completed hours against the weekly rhythm of
// its own history.
type Evaluator struct {
	db    *model.DB
	alert AlertRouter
	cfg   *MetrikaConfig
}

func NewEvaluator(db *model.DB, router AlertRouter, cfg *MetrikaConfig) *Evaluator {
	return &Evaluator{db: db, alert: router, cfg: cfg}
}

// minSamples is how many past occurrences of a slot a baseline needs before it
// is trusted. Below this the median says more about luck than about the site.
const minSamples = 2

// EvaluateWindow judges every enabled trigger of a counter for the measurement
// window ending at `end`, and reports how many alerts it delivered.
func (e *Evaluator) EvaluateWindow(ctx context.Context, counter *model.Counter, end time.Time) (int, error) {
	triggers, err := e.db.ListTriggers(ctx, counter.ID)
	if err != nil {
		return 0, fmt.Errorf("list triggers: %w", err)
	}

	enabled := make([]model.Trigger, 0, len(triggers))
	for _, t := range triggers {
		if t.Enabled {
			enabled = append(enabled, t)
		}
	}
	if len(enabled) == 0 {
		return 0, nil
	}

	// A filter applies to the whole request, so rules watching different pages
	// cannot share one. Rules that watch the same scope still do.
	groups, err := groupByScope(enabled)
	if err != nil {
		return 0, err
	}

	var fired int
	var firstErr error
	for _, scope := range groups {
		series, index, err := e.fetchSeries(ctx, counter, scope.triggers, end, scope.filter)
		if err != nil {
			log.Printf("counter %s: scope %q: %v", counter.CounterID, scope.label(), err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		for _, t := range scope.triggers {
			delivered, err := e.evaluateTrigger(ctx, counter, &t, series, index, end)
			if err != nil {
				log.Printf("counter %s: trigger %d (%s): %v", counter.CounterID, t.ID, t.Name, err)
				continue
			}
			if delivered {
				fired++
			}
		}
	}

	// Every scope failing means the window was not judged at all; the caller
	// must not advance its cursor past it.
	if fired == 0 && firstErr != nil {
		return 0, firstErr
	}
	return fired, nil
}

// scope is a set of rules that share one API request because they watch the
// same pages.
type scope struct {
	filter   string
	triggers []model.Trigger
}

func (s scope) label() string {
	if s.filter == "" {
		return "весь счётчик"
	}
	return s.filter
}

// groupByScope buckets rules by the filter they need, in a stable order so
// logs and tests read the same way every run.
func groupByScope(triggers []model.Trigger) ([]scope, error) {
	byFilter := make(map[string][]model.Trigger)
	for _, t := range triggers {
		filter, err := URLFilter(t.URLFilter, t.URLMatch)
		if err != nil {
			// One malformed rule must not take the counter's other rules down.
			log.Printf("trigger %d (%s): %v", t.ID, t.Name, err)
			continue
		}
		byFilter[filter] = append(byFilter[filter], t)
	}
	if len(byFilter) == 0 {
		return nil, fmt.Errorf("no rule has a usable scope")
	}

	filters := make([]string, 0, len(byFilter))
	for f := range byFilter {
		filters = append(filters, f)
	}
	sort.Strings(filters)

	out := make([]scope, 0, len(filters))
	for _, f := range filters {
		out = append(out, scope{filter: f, triggers: byFilter[f]})
	}
	return out, nil
}

// fetchSeries pulls the hourly history every trigger of this counter needs, in
// one request. The window has to reach back far enough to hold the deepest
// baseline any trigger asks for, and the metrics are deduplicated so a counter
// with ten visit triggers still costs one call.
func (e *Evaluator) fetchSeries(ctx context.Context, counter *model.Counter, triggers []model.Trigger, end time.Time, filter string) (*TimeSeries, map[string]int, error) {
	index := make(map[string]int)
	var metrics []string
	weeks := 1

	for _, t := range triggers {
		apiMetric, err := ResolveMetric(t.Metric)
		if err != nil {
			log.Printf("counter %s: trigger %d (%s): %v", counter.CounterID, t.ID, t.Name, err)
			continue
		}
		if _, seen := index[apiMetric]; !seen {
			index[apiMetric] = len(metrics)
			metrics = append(metrics, apiMetric)
		}
		if t.BaselineWeeks > weeks {
			weeks = t.BaselineWeeks
		}
	}
	if len(metrics) == 0 {
		return nil, nil, fmt.Errorf("no trigger names a usable metric")
	}
	// The API caps a request at 20 metrics.
	if len(metrics) > 20 {
		return nil, nil, fmt.Errorf("counter watches %d distinct metrics, the API allows 20 per request", len(metrics))
	}

	// Reach one day past the oldest week so the window at the far end is whole.
	from := end.AddDate(0, 0, -7*weeks-1)
	client := NewReportClient(counter, e.cfg.BaseURL)

	series, err := client.FetchByTime(ctx, Query{
		Metrics: metrics,
		Date1:   from.Format("2006-01-02"),
		Date2:   end.Format("2006-01-02"),
		Filters: filter,
	}, GroupTenMinutes)
	if err != nil {
		return nil, nil, err
	}

	// The API may coarsen a grouping it considers too fine for the range.
	// Reading a coarser series as if it were ten-minute buckets would compute
	// windows from the wrong spans, so this stops instead of guessing.
	if step := series.Step(); step != 0 && step != e.cfg.step() {
		return nil, nil, fmt.Errorf("Metrika returned %s intervals, expected %s — сократите baseline_weeks",
			step, e.cfg.step())
	}
	return series, index, nil
}

// evaluateTrigger judges one trigger and delivers an alert when it fires.
func (e *Evaluator) evaluateTrigger(ctx context.Context, counter *model.Counter, t *model.Trigger, series *TimeSeries, index map[string]int, end time.Time) (bool, error) {
	apiMetric, err := ResolveMetric(t.Metric)
	if err != nil {
		return false, err
	}
	metricIdx, ok := index[apiMetric]
	if !ok {
		return false, fmt.Errorf("metric %s missing from the series", apiMetric)
	}

	window := e.cfg.window()
	current, ok := WindowSum(series, metricIdx, end, window)
	if !ok {
		return false, fmt.Errorf("window ending %s is not covered by the returned series", end.Format(time.RFC3339))
	}

	baseline := BuildBaseline(series, metricIdx, end, window, t.BaselineWeeks)
	verdict := Judge(current, baseline, t.Direction, t.DeviationPct, t.MinBaseline, minSamples)

	if !verdict.Fired {
		return false, nil
	}
	if inCooldown, err := e.inCooldown(ctx, t); err != nil {
		return false, err
	} else if inCooldown {
		return false, nil
	}

	title := e.buildTitle(counter, t, verdict)
	message := e.buildMessage(t, verdict, end, window, series.Sampled)

	if err := e.alert.Alert(ctx, counter.ID, title, message); err != nil {
		log.Printf("counter %s: deliver alert for trigger %d: %v", counter.CounterID, t.ID, err)
	}

	record := &model.Alert{
		TriggerID: t.ID,
		CounterID: counter.ID,
		Title:     title,
		Message:   message,
		// The absolute figure that fired the alert, kept for the history view.
		EventCount: int(verdict.Current),
	}
	if err := e.db.CreateAlert(ctx, record); err != nil {
		log.Printf("counter %s: persist alert: %v", counter.CounterID, err)
	}
	if err := e.db.RecordTriggerFire(ctx, t.ID); err != nil {
		log.Printf("counter %s: record trigger fire: %v", counter.CounterID, err)
	}
	return true, nil
}

func (e *Evaluator) inCooldown(ctx context.Context, t *model.Trigger) (bool, error) {
	lastFired, ok, err := e.db.TriggerFiredAt(ctx, t.ID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	return time.Since(lastFired) < time.Duration(t.Cooldown)*time.Minute, nil
}

func (e *Evaluator) buildTitle(counter *model.Counter, t *model.Trigger, v Verdict) string {
	arrow := "📉"
	if v.DeviationPct > 0 {
		arrow = "📈"
	}
	return fmt.Sprintf("%s %s: %s %s на %.0f%%",
		arrow, counter.Name, MetricLabel(t.Metric), changeVerb(t.Metric, v.DeviationPct), absPct(v.DeviationPct))
}

// changeVerb agrees with the metric's own label. Every metric name is plural
// ("Визиты упали") except a single goal, which is feminine singular
// ("Цель 42 упала").
func changeVerb(metric string, deviationPct float64) string {
	singularFeminine := strings.HasPrefix(strings.ToLower(strings.TrimSpace(metric)), goalPrefix)

	if deviationPct > 0 {
		if singularFeminine {
			return "выросла"
		}
		return "выросли"
	}
	if singularFeminine {
		return "упала"
	}
	return "упали"
}

func (e *Evaluator) buildMessage(t *model.Trigger, v Verdict, end time.Time, window time.Duration, sampled bool) string {
	var b strings.Builder
	start := end.Add(-window)

	fmt.Fprintf(&b, "*Правило:* %s\n", t.Name)
	if strings.TrimSpace(t.URLFilter) != "" {
		fmt.Fprintf(&b, "*Область:* %s\n", URLFilterLabel(t.URLFilter, t.URLMatch))
	}
	fmt.Fprintf(&b, "*Окно:* %s–%s, %s\n",
		start.Format("02.01 15:04"), end.Format("15:04"), weekdayName(end.Weekday()))
	fmt.Fprintf(&b, "*%s:* %.0f\n", MetricLabel(t.Metric), v.Current)
	fmt.Fprintf(&b, "*Обычно в это время:* %.0f\n", v.Baseline.Value)
	fmt.Fprintf(&b, "*Отклонение:* %+.0f%% при пороге %d%%\n", v.DeviationPct, t.DeviationPct)
	fmt.Fprintf(&b, "\n_База — медиана %d последних %s в это же время._",
		v.Baseline.Samples, pluralWeekday(v.Baseline.Samples, end.Weekday()))

	if sampled {
		b.WriteString("\n_Данные семплированы._")
	}
	return b.String()
}

func absPct(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// pluralWeekday renders "вторников" / "вторника" so the alert text reads
// naturally for any sample count.
func pluralWeekday(n int, d time.Weekday) string {
	name := weekdayName(d)
	switch name {
	case "среда":
		name = "сред"
	case "пятница":
		name = "пятниц"
	case "суббота":
		name = "суббот"
	case "воскресенье":
		name = "воскресений"
	default:
		name += "ов"
	}
	return name
}
