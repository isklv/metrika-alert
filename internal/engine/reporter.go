package engine

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

// Reporter generates periodic comparison reports.
type Reporter struct {
	db    *model.DB
	cfg   *MetrikaConfig
	router AlertRouter
}

func NewReporter(db *model.DB, cfg *MetrikaConfig, router AlertRouter) *Reporter {
	return &Reporter{db: db, cfg: cfg, router: router}
}

// RunReports fetches current-period data for every counter and compares against yesterday/last week/last month.
func (r *Reporter) RunReports(ctx context.Context) error {
	counters, err := r.db.ListCounters(ctx)
	if err != nil {
		return fmt.Errorf("list counters: %w", err)
	}

	now := time.Now()
	for _, c := range counters {
		client := NewMetrikaClient(&c, r.cfg.BaseURL, r.cfg.LogsURL)
		if err := r.ReportCounter(ctx, &c, client, now); err != nil {
			log.Printf("report counter %d (%s): %v", c.ID, c.CounterID, err)
		}
	}
	return nil
}

func (r *Reporter) ReportCounter(ctx context.Context, counter *model.Counter, client *MetrikaClient, now time.Time) error {
	metrics := []string{"visits", "unique_visits", "pageviews", "bounce_rate", "avg_time_on_site", "avg_depth"}

	// Current hour snapshot.
	currentHour := now.Format("2006-01-02 15:00")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	lastWeek := now.AddDate(0, 0, -7).Format("2006-01-02")
	lastMonth := now.AddDate(0, -1, 0).Format("2006-01-02")

	currentData, err := client.FetchReport(ctx, currentHour, now.Format("2006-01-02 15:00"), metrics, 1)
	if err != nil {
		return fmt.Errorf("fetch current report: %w", err)
	}

	// Previous periods.
	yesterdayData, _ := client.FetchReport(ctx, yesterday+" "+now.Format("15:00"), yesterday+" 23:59", metrics, 1)
	lastWeekData, _ := client.FetchReport(ctx, lastWeek+" "+now.Format("15:00"), lastWeek+" 23:59", metrics, 1)
	lastMonthData, _ := client.FetchReport(ctx, lastMonth+" "+now.Format("15:00"), lastMonth+" 23:59", metrics, 1)

	// Goals.
	goalsCurrent, _ := client.FetchReport(ctx, currentHour, now.Format("2006-01-02 15:00"), []string{"goals"}, 50)

	// Save snapshot.
	snapshot := r.buildSnapshot(counter.ID, nil, "hour", now.Format("2006-01-02T15"), currentData, goalsCurrent)
	if err := r.db.CreateReportSnapshot(ctx, snapshot); err != nil {
		log.Printf("save snapshot: %v", err)
	}

	// Build comparison card.
	card := r.buildCard(counter.Name, currentData, yesterdayData, lastWeekData, lastMonthData, goalsCurrent)

	if err := r.router.Alert(ctx, counter.ID, "📊 Отчёт: "+counter.Name+" — "+now.Format("15:00"), card); err != nil {
		log.Printf("deliver report: %v", err)
	}

	return nil
}

func (r *Reporter) buildSnapshot(counterID int64, monitorID *int64, period, key string, data, goalsData []map[string]any) *model.ReportSnapshot {
	s := &model.ReportSnapshot{
		CounterID: counterID,
		MonitorID: monitorID,
		Period:    period,
		PeriodKey: key,
		TakenAt:   time.Now(),
		Goals:     make(map[string]int),
	}

	if len(data) == 0 {
		return s
	}
	row := data[0]
	if v, ok := row["visits"].(float64); ok && v > 0 {
		s.Visits = int(v)
	}
	if v, ok := row["unique_visits"].(float64); ok && v > 0 {
		s.UniqueVisits = int(v)
	}
	if v, ok := row["bounce_rate"].(float64); ok && v > 0 {
		s.Bounces = int(float64(s.Visits) * v / 100)
	}
	if v, ok := row["avg_time_on_site"].(float64); ok {
		s.AvgDuration = v
	}
	if v, ok := row["avg_depth"].(float64); ok {
		s.Depth = v
	}

	for _, g := range goalsData {
		if gid, ok := g["goal_id"].(float64); ok && gid > 0 {
			if cnt, ok := g["reaches"].(float64); ok && cnt > 0 {
				s.Goals[strconv.Itoa(int(gid))] = int(cnt)
			}
		}
	}

	return s
}

func (r *Reporter) buildCard(counterName string, current, yesterday, lastWeek, lastMonth, goals []map[string]any) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("*📊 Отчёт: %s — %s*\n\n", counterName, time.Now().Format("15:00")))

	if len(current) == 0 {
		b.WriteString("_Нет данных за текущий период._")
		return b.String()
	}

	row := current[0]
	b.WriteString(r.metricLine("Посещения", row["visits"], "visits", yesterday, lastWeek, lastMonth))
	b.WriteString(r.metricLine("Уникальные", row["unique_visits"], "unique_visits", yesterday, lastWeek, lastMonth))

	if v, ok := row["bounce_rate"].(float64); ok && v > 0 {
		b.WriteString(fmt.Sprintf("*Отказы:* %.0f%%\n", v))
	}
	if v, ok := row["avg_depth"].(float64); ok && v > 0 {
		b.WriteString(fmt.Sprintf("*Глубина:* %.1f\n", v))
	}

	if len(goals) > 0 {
		b.WriteString("\n*Цели:*\n")
		for _, g := range goals {
			if gid, ok := g["goal_id"].(float64); ok && gid > 0 {
				if cnt, ok := g["reaches"].(float64); ok && cnt > 0 {
					b.WriteString(fmt.Sprintf("  • %s: %.0f\n", strconv.Itoa(int(gid)), cnt))
				}
			}
		}
	}

	return b.String()
}

func (r *Reporter) metricLine(name string, currentAny any, key string, yesterday, lastWeek, lastMonth []map[string]any) string {
	cur, ok := currentAny.(float64)
	if !ok || cur <= 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("*%s:* %.0f", name, cur))

	for _, data := range [][]map[string]any{yesterday, lastWeek, lastMonth} {
		if len(data) > 0 {
			if v, ok := data[0][key].(float64); ok && v > 0 {
				pct := (cur - v) / v * 100
				arrow := "▲"
				if pct < 0 {
					arrow = "▼"
					pct = -pct
				}
				b.WriteString(fmt.Sprintf(" %s%.0f%%", arrow, pct))
			}
		}
	}
	b.WriteString("\n")
	return b.String()
}
