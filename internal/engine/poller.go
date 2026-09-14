package engine

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

// Poller walks each counter forward through measurement windows.
//
// The window is an hour wide but advances in ten-minute steps, so a drop is
// noticed within minutes instead of at the end of the hour, while the figure
// compared stays at hourly scale. Only settled data is judged: a window whose
// tail is still filling would read low against complete ones every time.
type Poller struct {
	db        *model.DB
	cfg       *MetrikaConfig
	evaluator *Evaluator

	mu      sync.Mutex
	started bool
	// emptyLogged keeps the "no counters" notice to the moment the set becomes
	// empty. Repeating it every reconcile would bury an idle install's log.
	emptyLogged bool
	// watched holds one entry per counter currently being polled. Counters are
	// added and removed as the database changes, so a counter created through
	// the bot starts being checked without restarting the service.
	watched map[int64]*watch
}

// watch is a running per-counter polling goroutine.
type watch struct {
	cancel context.CancelFunc
	// interval is what the goroutine was started with, so a change to the
	// counter's setting can be noticed and applied.
	interval time.Duration
}

// MetrikaConfig points the engine at the API and bounds how it reads history.
type MetrikaConfig struct {
	// BaseURL is the API host, https://api-metrika.yandex.net by default.
	BaseURL string
	// SettleMinutes is how long data is left to settle before it is judged.
	// Metrika keeps counting sessions for a short while after they happen, so
	// measuring right up to the present reads low.
	SettleMinutes int
	// WindowMinutes is how much traffic one measurement covers. An hour keeps
	// the figure stable; the window still advances every StepMinutes.
	WindowMinutes int
	// StepMinutes is how far the window advances per check, and the granularity
	// requested from the API.
	StepMinutes int
	// MaxCatchUpSteps bounds how many missed windows one tick works through, so
	// a counter idle for a week does not fire a burst of stale alerts at once.
	MaxCatchUpSteps int
}

// Defaults for MetrikaConfig.
const (
	DefaultSettleMinutes   = 20
	DefaultWindowMinutes   = 60
	DefaultStepMinutes     = 10
	DefaultMaxCatchUpSteps = 6
)

func (c *MetrikaConfig) settle() time.Duration {
	if c.SettleMinutes <= 0 {
		return DefaultSettleMinutes * time.Minute
	}
	return time.Duration(c.SettleMinutes) * time.Minute
}

func (c *MetrikaConfig) window() time.Duration {
	if c.WindowMinutes <= 0 {
		return DefaultWindowMinutes * time.Minute
	}
	return time.Duration(c.WindowMinutes) * time.Minute
}

// step is both the cadence and the API granularity, so the two can never drift
// apart into windows that do not line up with the buckets they are summed from.
func (c *MetrikaConfig) step() time.Duration {
	if c.StepMinutes <= 0 {
		return DefaultStepMinutes * time.Minute
	}
	return time.Duration(c.StepMinutes) * time.Minute
}

func (c *MetrikaConfig) maxCatchUp() int {
	if c.MaxCatchUpSteps <= 0 {
		return DefaultMaxCatchUpSteps
	}
	return c.MaxCatchUpSteps
}

func NewPoller(db *model.DB, cfg *MetrikaConfig, evaluator *Evaluator) *Poller {
	return &Poller{db: db, cfg: cfg, evaluator: evaluator, watched: make(map[int64]*watch)}
}

// reconcileInterval is how often the set of polled counters is compared with
// the database. It only costs one local query, so it can be brisk: this is the
// delay between adding a counter and the service starting to check it.
const reconcileInterval = 30 * time.Second

// Start begins polling and returns immediately.
//
// The set of counters is reconciled with the database on a timer rather than
// read once, so counters added, removed or retuned through the bot or the API
// take effect on their own — restarting the service to pick up a new counter
// was a wart, and a deleted one used to leave its goroutine running forever.
func (p *Poller) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return fmt.Errorf("poller already running")
	}
	p.started = true
	p.mu.Unlock()

	// Reconcile once inline so a broken database surfaces at startup rather
	// than as a log line half a minute later.
	if err := p.reconcile(ctx); err != nil {
		return err
	}
	log.Printf("poller started: %s window stepped every %s, %s to settle, счётчики подхватываются на лету",
		p.cfg.window(), p.cfg.step(), p.cfg.settle())

	go p.supervise(ctx)
	return nil
}

// supervise keeps the polled set in step with the database until ctx ends.
func (p *Poller) supervise(ctx context.Context) {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.stopAll()
			return
		case <-ticker.C:
			if err := p.reconcile(ctx); err != nil && ctx.Err() == nil {
				log.Printf("poller: reconcile: %v", err)
			}
		}
	}
}

// reconcile starts goroutines for counters that have none, stops those whose
// counter is gone, and restarts any whose interval changed.
func (p *Poller) reconcile(ctx context.Context) error {
	counters, err := p.db.ListCounters(ctx)
	if err != nil {
		return fmt.Errorf("list counters: %w", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	present := make(map[int64]bool, len(counters))
	for _, c := range counters {
		present[c.ID] = true
		interval := pollInterval(c)

		if existing, ok := p.watched[c.ID]; ok {
			if existing.interval == interval {
				continue
			}
			// The cadence changed; replace the goroutine with one that ticks
			// at the new rate.
			existing.cancel()
			delete(p.watched, c.ID)
			log.Printf("counter %s: интервал проверки изменён на %s", c.CounterID, interval)
		}

		counterCtx, cancel := context.WithCancel(ctx)
		p.watched[c.ID] = &watch{cancel: cancel, interval: interval}
		go p.pollCounterLoop(counterCtx, c, interval)
		log.Printf("counter %s: проверка запущена, каждые %s", c.CounterID, interval)
	}

	for id, w := range p.watched {
		if present[id] {
			continue
		}
		w.cancel()
		delete(p.watched, id)
		log.Printf("counter %d: удалён, проверка остановлена", id)
	}

	switch {
	case len(p.watched) == 0 && !p.emptyLogged:
		log.Printf("poller: счётчиков нет — добавьте первый через /addcounter")
		p.emptyLogged = true
	case len(p.watched) > 0:
		p.emptyLogged = false
	}
	return nil
}

// stopAll ends every polling goroutine.
func (p *Poller) stopAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, w := range p.watched {
		w.cancel()
		delete(p.watched, id)
	}
}

// Watched reports how many counters are currently being polled.
func (p *Poller) Watched() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.watched)
}

// pollInterval is how often a counter is checked, floored at a minute.
func pollInterval(c model.Counter) time.Duration {
	interval := time.Duration(c.PollInterval) * time.Minute
	if interval < time.Minute {
		interval = time.Minute
	}
	return interval
}

func (p *Poller) pollCounterLoop(ctx context.Context, c model.Counter, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Check straight away: a counter added a moment ago should not wait a full
	// interval before its first look, which is what made adding one feel inert.
	if err := p.advance(ctx, c.ID); err != nil && ctx.Err() == nil {
		log.Printf("poll counter %d (%s): %v", c.ID, c.CounterID, err)
	}

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

// advance judges every window a counter has not seen yet.
func (p *Poller) advance(ctx context.Context, counterID int64) error {
	// Re-read the counter each tick: its cursor moves, and its token or
	// interval may have been changed through the bot or the API.
	counter, err := p.db.GetCounter(ctx, counterID)
	if err != nil {
		return err
	}

	windows := p.pendingWindows(counter, time.Now())
	if len(windows) == 0 {
		return nil
	}

	for _, end := range windows {
		fired, err := p.evaluator.EvaluateWindow(ctx, counter, end)
		if err != nil {
			// Leave the cursor where it is so the window is retried rather than
			// silently skipped.
			return fmt.Errorf("judge window ending %s: %w", end.Format("2006-01-02 15:04"), err)
		}
		if err := p.db.SetLastHourChecked(ctx, counter.ID, end); err != nil {
			return err
		}
		if fired > 0 {
			log.Printf("counter %s: window ending %s fired %d alert(s)",
				counter.CounterID, end.Format("2006-01-02 15:04"), fired)
		}
	}
	return nil
}

// pendingWindows lists the window ends a counter still owes, oldest first.
func (p *Poller) pendingWindows(counter *model.Counter, now time.Time) []time.Time {
	step := p.cfg.step()

	// The newest window end whose data has settled. Everything up to this point
	// is counted; measuring closer to the present reads low.
	latest := truncateTo(now.Add(-p.cfg.settle()), step)

	// A counter with no cursor starts at the latest window rather than replaying
	// history: those windows are past and alerting on them helps nobody.
	if counter.LastHourChecked == nil {
		return []time.Time{latest}
	}

	var windows []time.Time
	for end := truncateTo(*counter.LastHourChecked, step).Add(step); !end.After(latest); end = end.Add(step) {
		windows = append(windows, end)
		if len(windows) >= p.cfg.maxCatchUp() {
			break
		}
	}
	return windows
}

// truncateTo rounds t down to a multiple of step within the day. time.Truncate
// works on absolute time, which drifts against local wall-clock slots when the
// zone offset is not a whole number of hours.
func truncateTo(t time.Time, step time.Duration) time.Time {
	minutes := t.Hour()*60 + t.Minute()
	stepMinutes := int(step / time.Minute)
	if stepMinutes <= 0 {
		stepMinutes = 1
	}
	aligned := minutes - minutes%stepMinutes

	return time.Date(t.Year(), t.Month(), t.Day(), aligned/60, aligned%60, 0, 0, t.Location())
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
