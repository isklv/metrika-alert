package engine

import (
	"context"
	"log"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

// ReportScheduler sends periodic reports on the schedule stored in the database.
//
// The schedule is re-read every tick rather than captured at start, so changing
// it from a chat takes effect without restarting the service.
type ReportScheduler struct {
	db       *model.DB
	reporter *Reporter
	// fallback applies until a schedule is stored, so an install configured
	// only through config.yaml keeps working.
	fallback Schedule
}

func NewReportScheduler(db *model.DB, reporter *Reporter, fallback Schedule) *ReportScheduler {
	return &ReportScheduler{db: db, reporter: reporter, fallback: fallback}
}

// checkInterval is how often the schedule is evaluated. A minute is fine enough
// for a daily time and costs one local query.
const checkInterval = time.Minute

// Schedule returns the schedule in force.
func (s *ReportScheduler) Schedule(ctx context.Context) Schedule {
	raw, ok, err := s.db.Setting(ctx, model.SettingReportSchedule)
	if err != nil {
		log.Printf("reports: read schedule: %v", err)
		return s.fallback
	}
	if !ok {
		return s.fallback
	}

	schedule, err := ParseSchedule(raw)
	if err != nil {
		log.Printf("reports: stored schedule %q is unreadable, falling back: %v", raw, err)
		return s.fallback
	}
	return schedule
}

// SetSchedule stores a new schedule. It takes effect within a minute.
func (s *ReportScheduler) SetSchedule(ctx context.Context, schedule Schedule) error {
	return s.db.SetSetting(ctx, model.SettingReportSchedule, schedule.String())
}

// NextRun reports when the next report is due, if any.
func (s *ReportScheduler) NextRun(ctx context.Context) (time.Time, bool) {
	schedule := s.Schedule(ctx)
	last, err := s.db.SettingTime(ctx, model.SettingReportLastRun)
	if err != nil {
		return time.Time{}, false
	}
	return schedule.Next(last, time.Now())
}

// Run evaluates the schedule until ctx ends.
func (s *ReportScheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	log.Printf("reports: %s", s.Schedule(ctx).Describe())

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx, time.Now())
		}
	}
}

// tick sends a report if one is due.
func (s *ReportScheduler) tick(ctx context.Context, now time.Time) {
	schedule := s.Schedule(ctx)
	if schedule.Off() {
		return
	}

	last, err := s.db.SettingTime(ctx, model.SettingReportLastRun)
	if err != nil {
		log.Printf("reports: read last run: %v", err)
		return
	}
	if !schedule.Due(last, now) {
		return
	}

	// Record the run before sending. A failure mid-report must not queue the
	// same report again on the next tick and every tick after it.
	if err := s.db.SetSettingTime(ctx, model.SettingReportLastRun, now); err != nil {
		log.Printf("reports: record run: %v", err)
		return
	}

	if err := s.reporter.RunReports(ctx); err != nil {
		log.Printf("reports: %v", err)
	}
}
