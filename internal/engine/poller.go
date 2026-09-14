package engine

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

// Poller walks each counter forward through completed hours.
//
// Only whole hours are judged: the hour in progress is still filling, and
// comparing a partial hour against complete ones would report a drop every time.
type Poller struct {
	db        *model.DB
	cfg       *MetrikaConfig
	evaluator *Evaluator

	mu      sync.Mutex
	running bool
}

// MetrikaConfig points the engine at the API and bounds how it reads history.
type MetrikaConfig struct {
	// BaseURL is the API host, https://api-metrika.yandex.net by default.
	BaseURL string
	// SettleMinutes is how long after an hour ends before it is judged. Metrika
	// keeps counting sessions for a short while after the boundary, so judging
	// an hour the moment it closes reads low.
	SettleMinutes int
	// MaxCatchUpHours bounds how many missed hours one tick works through, so a
	// counter idle for a week does not fire a burst of stale alerts at once.
	MaxCatchUpHours int
}

// Defaults for MetrikaConfig.
const (
	DefaultSettleMinutes   = 20
	DefaultMaxCatchUpHours = 6
)

func (c *MetrikaConfig) settle() time.Duration {
	if c.SettleMinutes <= 0 {
		return DefaultSettleMinutes * time.Minute
	}
	return time.Duration(c.SettleMinutes) * time.Minute
}

func (c *MetrikaConfig) maxCatchUp() int {
	if c.MaxCatchUpHours <= 0 {
		return DefaultMaxCatchUpHours
	}
	return c.MaxCatchUpHours
}

func NewPoller(db *model.DB, cfg *MetrikaConfig, evaluator *Evaluator) *Poller {
	return &Poller{db: db, cfg: cfg, evaluator: evaluator}
}

// Start launches one goroutine per counter and returns immediately.
func (p *Poller) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return fmt.Errorf("poller already running")
	}
	p.running = true
	p.mu.Unlock()

	counters, err := p.db.ListCounters(ctx)
	if err != nil {
		return fmt.Errorf("list counters: %w", err)
	}
	if len(counters) == 0 {
		log.Printf("poller started: no counters configured — add one with /addcounter")
		return nil
	}

	for _, c := range counters {
		go p.pollCounterLoop(ctx, c)
	}
	log.Printf("poller started: %d counter(s), hours judged %s after they close",
		len(counters), p.cfg.settle())
	return nil
}

func (p *Poller) pollCounterLoop(ctx context.Context, c model.Counter) {
	interval := time.Duration(c.PollInterval) * time.Minute
	if interval < time.Minute {
		interval = time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.advance(ctx, c.ID); err != nil && ctx.Err() == nil {
				log.Printf("poll counter %d (%s): %v", c.ID, c.CounterID, err)
			}
		}
	}
}

// advance judges every hour a counter has not seen yet.
func (p *Poller) advance(ctx context.Context, counterID int64) error {
	// Re-read the counter each tick: its cursor moves, and its token or
	// interval may have been changed through the bot or the API.
	counter, err := p.db.GetCounter(ctx, counterID)
	if err != nil {
		return err
	}

	hours := p.pendingHours(counter, time.Now())
	if len(hours) == 0 {
		return nil
	}

	for _, hour := range hours {
		fired, err := p.evaluator.EvaluateHour(ctx, counter, hour)
		if err != nil {
			// Leave the cursor where it is so the hour is retried rather than
			// silently skipped.
			return fmt.Errorf("judge hour %s: %w", hour.Format("2006-01-02 15:00"), err)
		}
		if err := p.db.SetLastHourChecked(ctx, counter.ID, hour); err != nil {
			return err
		}
		if fired > 0 {
			log.Printf("counter %s: hour %s fired %d alert(s)",
				counter.CounterID, hour.Format("2006-01-02 15:00"), fired)
		}
	}
	return nil
}

// pendingHours lists the completed hours a counter still owes, oldest first.
func (p *Poller) pendingHours(counter *model.Counter, now time.Time) []time.Time {
	// The newest hour that has both closed and settled.
	//
	// The hour starting at H only closes at H+1h, so subtracting the settle
	// delay alone would hand back the hour still in progress — and a partial
	// hour measured against complete ones reads as a drop every single time.
	latest := now.Add(-time.Hour - p.cfg.settle()).Truncate(time.Hour)

	// A counter with no cursor starts at the previous hour rather than replaying
	// history: those hours are long past and alerting on them helps nobody.
	if counter.LastHourChecked == nil {
		return []time.Time{latest}
	}

	var hours []time.Time
	for h := counter.LastHourChecked.Truncate(time.Hour).Add(time.Hour); !h.After(latest); h = h.Add(time.Hour) {
		hours = append(hours, h)
		if len(hours) >= p.cfg.maxCatchUp() {
			break
		}
	}
	return hours
}

// RunOnce advances every counter a single step. Used by /poll and the REST API.
func (p *Poller) RunOnce(ctx context.Context) error {
	counters, err := p.db.ListCounters(ctx)
	if err != nil {
		return fmt.Errorf("list counters: %w", err)
	}
	if len(counters) == 0 {
		return fmt.Errorf("no counters configured")
	}

	var firstErr error
	for _, c := range counters {
		if err := p.advance(ctx, c.ID); err != nil {
			log.Printf("run-once counter %d (%s): %v", c.ID, c.CounterID, err)
			if firstErr == nil {
				firstErr = fmt.Errorf("counter %s: %w", c.CounterID, err)
			}
		}
	}
	return firstErr
}
