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

// RunReports reports on every counter.
func (r *Reporter) RunReports(ctx context.Context) error {
	counters, err := r.db.ListCounters(ctx)
	if err != nil {
		return fmt.Errorf("list counters: %w", err)
	}
	if len(counters) == 0 {
		return fmt.Errorf("no counters configured")
	}

	now := time.Now()
	var firstErr error
	for _, c := range counters {
		client := NewReportClient(&c, r.cfg.BaseURL)
		if err := r.ReportCounter(ctx, &c, client, now); err != nil {
			log.Printf("report counter %d (%s): %v", c.ID, c.CounterID, err)
			if firstErr == nil {
				firstErr = fmt.Errorf("counter %s: %w", c.CounterID, err)
			}
		}
	}
	return firstErr
}

// RunReportFor builds and delivers the report for a single counter.
func (r *Reporter) RunReportFor(ctx context.Context, counterID int64) error {
	counter, err := r.db.GetCounter(ctx, counterID)
	if err != nil {
		return err
	}
	return r.ReportCounter(ctx, counter, NewReportClient(counter, r.cfg.BaseURL), time.Now())
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

// ReportCounter fetches today's figures, compares them with earlier periods and
// delivers the card.
func (r *Reporter) ReportCounter(ctx context.Context, counter *model.Counter, client *ReportClient, now time.Time) error {
	current, err := client.Fetch(ctx, Query{Metrics: SummaryMetrics, Date1: "today", Date2: "today"})
	if err != nil {
		return fmt.Errorf("fetch today: %w", err)
	}

	// Earlier periods are context, not the subject: a counter with no data a
	// month ago should still produce today's report.
	past := make(map[string]summary, len(comparisons))
	for _, p := range comparisons {
		res, err := client.Fetch(ctx, Query{Metrics: SummaryMetrics, Date1: p.date1, Date2: p.date2})
		if err != nil {
			log.Printf("counter %s: fetch %s: %v", counter.CounterID, p.label, err)
			continue
		}
		past[p.label] = summaryFrom(res)
	}

	today := summaryFrom(current)
	goals := r.goalBreakdown(ctx, counter, client)

	if err := r.saveSnapshot(ctx, counter.ID, now, today, goals); err != nil {
		log.Printf("counter %s: save snapshot: %v", counter.CounterID, err)
	}

	title := fmt.Sprintf("📊 Отчёт: %s — %s", counter.Name, now.Format("02.01 15:04"))
	card := r.buildCard(today, past, goals, current.Sampled)

	return r.router.Alert(ctx, counter.ID, title, card)
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
func (r *Reporter) goalBreakdown(ctx context.Context, counter *model.Counter, client *ReportClient) []goalEntry {
	goals, err := client.Goals(ctx)
	if err != nil {
		log.Printf("counter %s: goal list unavailable, reporting totals only: %v", counter.CounterID, err)
		return nil
	}
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

	res, err := client.Fetch(ctx, Query{Metrics: metrics, Date1: "today", Date2: "today"})
	if err != nil {
		log.Printf("counter %s: fetch goal reaches: %v", counter.CounterID, err)
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
func (r *Reporter) buildCard(today summary, past map[string]summary, goals []goalEntry, sampled bool) string {
	var b strings.Builder

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
