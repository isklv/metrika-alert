package engine

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

// Reporter builds periodic comparison reports from the Reporting API.
//
// Reports use the aggregated API rather than the Logs API because it answers
// in one call and accepts date2=today — the Logs API cannot report on the day
// that is still in progress.
type Reporter struct {
	db     *model.DB
	cfg    *MetrikaConfig
	router AlertRouter
}

func NewReporter(db *model.DB, cfg *MetrikaConfig, router AlertRouter) *Reporter {
	return &Reporter{db: db, cfg: cfg, router: router}
}

// period is one comparison window, expressed in the API's own relative date
// keywords. Comparing whole equivalent days is what makes the arrows
// meaningful: a part-day "today" against a full "yesterday" would show a
// decline every morning regardless of traffic.
type period struct {
	label string
	date1 string
	date2 string
}

var comparisons = []period{
	{"вчера", "yesterday", "yesterday"},
	{"неделю назад", "7daysAgo", "7daysAgo"},
	{"месяц назад", "30daysAgo", "30daysAgo"},
}

// RunReports sends every configured report.
func (r *Reporter) RunReports(ctx context.Context) error {
	return r.runReports(ctx, 0)
}

// RunReportFor sends the reports belonging to one counter.
func (r *Reporter) RunReportFor(ctx context.Context, counterID int64) error {
	if _, err := r.db.GetCounter(ctx, counterID); err != nil {
		return err
	}
	return r.runReports(ctx, counterID)
}

func (r *Reporter) runReports(ctx context.Context, counterID int64) error {
	counters, err := r.db.ListCounters(ctx)
	if err != nil {
		return fmt.Errorf("list counters: %w", err)
	}
	if len(counters) == 0 {
		return fmt.Errorf("no counters configured")
	}

	byID := make(map[int64]model.Counter, len(counters))
	for _, c := range counters {
		byID[c.ID] = c
	}

	reports, err := r.reportsToSend(ctx, counterID, counters)
	if err != nil {
		return err
	}

	now := time.Now()
	var firstErr error
	for _, report := range reports {
		counter, ok := byID[report.CounterID]
		if !ok {
			continue
		}
		client := NewReportClient(&counter, r.cfg.BaseURL)
		if err := r.ReportOne(ctx, &counter, &report, client, now); err != nil {
			log.Printf("report %q (counter %s): %v", report.Name, counter.CounterID, err)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", report.Name, err)
			}
		}
	}
	return firstErr
}

// reportsToSend returns the configured reports, or one whole-counter report per
// counter when none are configured — which is what the service always did and
// what an install that never touched /addreport should keep getting.
func (r *Reporter) reportsToSend(ctx context.Context, counterID int64, counters []model.Counter) ([]model.Report, error) {
	configured, err := r.db.ListReports(ctx, counterID)
	if err != nil {
		return nil, err
	}

	var enabled []model.Report
	for _, report := range configured {
		if report.Enabled {
			enabled = append(enabled, report)
		}
	}
	if len(enabled) > 0 {
		return enabled, nil
	}

	for _, c := range counters {
		if counterID > 0 && c.ID != counterID {
			continue
		}
		enabled = append(enabled, model.Report{CounterID: c.ID, Name: c.Name, Enabled: true})
	}
	return enabled, nil
}

// summary is one period's figures, in SummaryMetrics order.
type summary struct {
	visits      float64
	users       float64
	pageviews   float64
	bounceRate  float64
	pageDepth   float64
	avgDuration float64
	goalReaches float64
	present     bool
}

func summaryFrom(res *Result) summary {
	if res == nil || res.Empty() {
		return summary{}
	}
	return summary{
		visits:      res.Total(0),
		users:       res.Total(1),
		pageviews:   res.Total(2),
		bounceRate:  res.Total(3),
		pageDepth:   res.Total(4),
		avgDuration: res.Total(5),
		goalReaches: res.Total(6),
		present:     true,
	}
}

// ReportOne fetches today's figures for one report definition, compares them
// with earlier periods and delivers the card.
func (r *Reporter) ReportOne(ctx context.Context, counter *model.Counter, report *model.Report, client *ReportClient, now time.Time) error {
	filter, err := URLFilter(report.URLFilter, report.URLMatch)
	if err != nil {
		return err
	}

	current, err := client.Fetch(ctx, Query{
		Metrics: SummaryMetrics, Date1: "today", Date2: "today", Filters: filter,
	})
	if err != nil {
		return fmt.Errorf("fetch today: %w", err)
	}

	// Earlier periods are context, not the subject: a page with no data a month
	// ago should still produce today's report.
	past := make(map[string]summary, len(comparisons))
	for _, p := range comparisons {
		res, err := client.Fetch(ctx, Query{
			Metrics: SummaryMetrics, Date1: p.date1, Date2: p.date2, Filters: filter,
		})
		if err != nil {
			log.Printf("report %q: fetch %s: %v", report.Name, p.label, err)
			continue
		}
		past[p.label] = summaryFrom(res)
	}

	today := summaryFrom(current)
	goals := r.goalBreakdown(ctx, counter, client, report, filter)

	if err := r.saveSnapshot(ctx, counter.ID, now, today, goals); err != nil {
		log.Printf("report %q: save snapshot: %v", report.Name, err)
	}

	title := fmt.Sprintf("📊 %s — %s", reportTitle(counter, report), now.Format("02.01 15:04"))
	card := r.buildCard(today, past, goals, bool(current.Sampled), report)

	return r.router.Alert(ctx, counter.ID, title, card)
}

// reportTitle names the report, falling back to the counter for the
// whole-counter default.
func reportTitle(counter *model.Counter, report *model.Report) string {
	if report.Name != "" && report.Name != counter.Name {
		return counter.Name + " · " + report.Name
	}
	return counter.Name
}

// goalEntry is one goal's reach count for the report.
type goalEntry struct {
	name    string
	reaches float64
}

// maxGoalsInReport bounds both the API request and the card. The API allows 20
// metrics per request, and a card listing dozens of goals is unreadable anyway.
const maxGoalsInReport = 15

// goalBreakdown resolves per-goal conversions. It is best effort: a token
// without management access still yields a report, just without the breakdown.
func (r *Reporter) goalBreakdown(ctx context.Context, counter *model.Counter, client *ReportClient, report *model.Report, filter string) []goalEntry {
	goals := r.goalsToReport(ctx, counter, report)
	if len(goals) == 0 {
		return nil
	}
	if len(goals) > maxGoalsInReport {
		goals = goals[:maxGoalsInReport]
	}

	metrics := make([]string, 0, len(goals))
	for _, g := range goals {
		metrics = append(metrics, GoalReachesMetric(g.ID))
	}

	res, err := client.Fetch(ctx, Query{Metrics: metrics, Date1: "today", Date2: "today", Filters: filter})
	if err != nil {
		log.Printf("report %q: fetch goal reaches: %v", report.Name, err)
		return nil
	}

	entries := make([]goalEntry, 0, len(goals))
	for i, g := range goals {
		reaches := res.Total(i)
		if reaches <= 0 {
			continue
		}
		name := g.Name
		if name == "" {
			name = "цель " + strconv.FormatInt(g.ID, 10)
		}
		entries = append(entries, goalEntry{name: name, reaches: reaches})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].reaches > entries[j].reaches })
	return entries
}

// goalsToReport resolves which goals the report covers.
//
// When the report names goal IDs, those are what it reports — and it does so
// without needing the management API, so a token that cannot read the counter's
// settings still produces a goal breakdown. Names are filled in when available.
func (r *Reporter) goalsToReport(ctx context.Context, counter *model.Counter, report *model.Report) []Goal {
	named, err := NewReportClient(counter, r.cfg.BaseURL).Goals(ctx)
	if err != nil {
		if len(report.GoalIDs) == 0 {
			log.Printf("report %q: goal list unavailable, reporting totals only: %v", report.Name, err)
			return nil
		}
		log.Printf("report %q: goal names unavailable, reporting by ID: %v", report.Name, err)
	}

	if len(report.GoalIDs) == 0 {
		return named
	}

	byID := make(map[int64]Goal, len(named))
	for _, g := range named {
		byID[g.ID] = g
	}

	selected := make([]Goal, 0, len(report.GoalIDs))
	for _, id := range report.GoalIDs {
		if g, ok := byID[id]; ok {
			selected = append(selected, g)
			continue
		}
		selected = append(selected, Goal{ID: id})
	}
	return selected
}

func (r *Reporter) saveSnapshot(ctx context.Context, counterID int64, now time.Time, s summary, goals []goalEntry) error {
	goalCounts := make(map[string]int, len(goals))
	for _, g := range goals {
		goalCounts[g.name] = int(g.reaches)
	}

	return r.db.CreateReportSnapshot(ctx, &model.ReportSnapshot{
		CounterID:    counterID,
		Period:       "hour",
		PeriodKey:    now.Format("2006-01-02T15"),
		TakenAt:      now,
		Visits:       int(s.visits),
		UniqueVisits: int(s.users),
		// bounceRate is a percentage; the snapshot stores an absolute count.
		Bounces:     int(s.visits * s.bounceRate / 100),
		AvgDuration: s.avgDuration,
		Depth:       s.pageDepth,
		Goals:       goalCounts,
	})
}

// buildCard renders the report.
func (r *Reporter) buildCard(today summary, past map[string]summary, goals []goalEntry, sampled bool, report *model.Report) string {
	var b strings.Builder

	if scope := URLFilterLabel(report.URLFilter, report.URLMatch); report.URLFilter != "" {
		b.WriteString("_" + scope + "_\n\n")
	}

	if !today.present || today.visits == 0 {
		b.WriteString("_Сегодня данных пока нет._")
		return b.String()
	}

	b.WriteString(metricLine("Визиты", today.visits, past, func(s summary) float64 { return s.visits }, "%.0f"))
	b.WriteString(metricLine("Посетители", today.users, past, func(s summary) float64 { return s.users }, "%.0f"))
	b.WriteString(metricLine("Просмотры", today.pageviews, past, func(s summary) float64 { return s.pageviews }, "%.0f"))

	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("*Отказы:* %.1f%%\n", today.bounceRate))
	b.WriteString(fmt.Sprintf("*Глубина:* %.1f стр.\n", today.pageDepth))
	b.WriteString(fmt.Sprintf("*Время на сайте:* %s\n", formatDuration(today.avgDuration)))

	if today.goalReaches > 0 {
		b.WriteString(fmt.Sprintf("*Достижений целей:* %.0f\n", today.goalReaches))
	}

	if len(goals) > 0 {
		b.WriteString("\n*Цели:*\n")
		for _, g := range goals {
			b.WriteString(fmt.Sprintf("  • %s: %.0f\n", g.name, g.reaches))
		}
	}

	if sampled {
		b.WriteString("\n_Данные семплированы._")
	}
	return b.String()
}

// metricLine renders one metric with its change against each earlier period.
func metricLine(name string, current float64, past map[string]summary, pick func(summary) float64, format string) string {
	var b strings.Builder
	b.WriteString("*" + name + ":* ")
	b.WriteString(fmt.Sprintf(format, current))

	var deltas []string
	for _, p := range comparisons {
		s, ok := past[p.label]
		if !ok || !s.present {
			continue
		}
		previous := pick(s)
		if previous <= 0 {
			continue
		}
		deltas = append(deltas, p.label+" "+formatDelta(current, previous))
	}
	if len(deltas) > 0 {
		b.WriteString("  (" + strings.Join(deltas, ", ") + ")")
	}
	b.WriteString("\n")
	return b.String()
}

// formatDelta renders a change as an arrow and a percentage.
func formatDelta(current, previous float64) string {
	pct := (current - previous) / previous * 100
	switch {
	case pct >= 0.5:
		return fmt.Sprintf("▲%.0f%%", pct)
	case pct <= -0.5:
		return fmt.Sprintf("▼%.0f%%", -pct)
	default:
		return "≈"
	}
}

// formatDuration renders seconds on site as mm:ss.
func formatDuration(seconds float64) string {
	d := time.Duration(seconds) * time.Second
	return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}
