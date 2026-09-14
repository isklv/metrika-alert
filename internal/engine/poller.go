package engine

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

// EventConsumer receives batches of events per counter.
type EventConsumer func(counterID int64, events []*model.MetrikaEvent) error

// Poller drives the Logs API export lifecycle for every counter.
//
// The Logs API is asynchronous, so a poll tick does not fetch events — it
// advances a state machine:
//
//	no export        → order one for the window after the watermark
//	export pending   → ask Metrika whether it is ready
//	export processed → download, evaluate, advance watermark, clean up
//	export failed    → drop it so the next tick orders a fresh one
//
// That is also why the export is recorded in the database: it routinely
// outlives the tick that ordered it, and an untracked one would sit against
// the counter's quota forever.
type Poller struct {
	db       *model.DB
	cfg      *MetrikaConfig
	consumer EventConsumer

	mu      sync.Mutex
	running bool
}

// MetrikaConfig points the engine at the API and bounds how far back it looks.
type MetrikaConfig struct {
	// BaseURL is the API host, https://api-metrika.yandex.net by default.
	BaseURL string
	// LagHours is how far behind now an export window must end. The Logs API
	// rejects date2 = today outright, and recent data keeps arriving for a
	// while, so a window that ends too close to now returns partial results.
	LagHours int
	// MaxWindowHours caps a single export so a long outage is caught up in
	// bounded steps instead of one request Metrika refuses on quota.
	MaxWindowHours int
}

// Defaults for MetrikaConfig.
const (
	DefaultLagHours       = 26
	DefaultMaxWindowHours = 24
)

func (c *MetrikaConfig) lag() time.Duration {
	if c.LagHours <= 0 {
		return DefaultLagHours * time.Hour
	}
	return time.Duration(c.LagHours) * time.Hour
}

func (c *MetrikaConfig) maxWindow() time.Duration {
	if c.MaxWindowHours <= 0 {
		return DefaultMaxWindowHours * time.Hour
	}
	return time.Duration(c.MaxWindowHours) * time.Hour
}

func NewPoller(db *model.DB, cfg *MetrikaConfig, consumer EventConsumer) *Poller {
	return &Poller{db: db, cfg: cfg, consumer: consumer}
}

// staleAfter is how long an export may sit untouched before it is abandoned.
// A process killed mid-flight leaves one behind; nothing will ever download it.
const staleAfter = 6 * time.Hour

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

	// Exports orphaned by an earlier run hold quota that new ones need.
	p.reapStaleRequests(ctx, counters)

	if len(counters) == 0 {
		log.Printf("poller started: no counters configured — add one with /addcounter")
		return nil
	}

	for _, c := range counters {
		go p.pollCounterLoop(ctx, c)
	}
	log.Printf("poller started: %d counter(s), export window ends %s behind now",
		len(counters), p.cfg.lag())
	return nil
}

// reapStaleRequests cancels exports left behind by a previous process.
func (p *Poller) reapStaleRequests(ctx context.Context, counters []model.Counter) {
	stale, err := p.db.StaleLogRequests(ctx, time.Now().Add(-staleAfter))
	if err != nil {
		log.Printf("poller: list stale log requests: %v", err)
		return
	}

	byID := make(map[int64]model.Counter, len(counters))
	for _, c := range counters {
		byID[c.ID] = c
	}

	for _, r := range stale {
		counter, ok := byID[r.CounterID]
		if !ok {
			p.db.DeleteLogRequest(ctx, r.ID)
			continue
		}
		client := NewLogsClient(&counter, p.cfg.BaseURL)
		// Best effort: Metrika may have expired it already, and either way the
		// local row must go so the next tick can order a fresh export.
		if err := client.Cancel(ctx, r.RequestID); err != nil {
			log.Printf("poller: cancel stale export %d for counter %s: %v", r.RequestID, counter.CounterID, err)
		}
		if err := p.db.DeleteLogRequest(ctx, r.ID); err != nil {
			log.Printf("poller: drop stale export %d: %v", r.RequestID, err)
		} else {
			log.Printf("poller: abandoned stale export %d for counter %s", r.RequestID, counter.CounterID)
		}
	}
}

// pollCounterLoop advances one counter's state machine at its interval.
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

// advance moves one counter one step through the export lifecycle.
func (p *Poller) advance(ctx context.Context, counterID int64) error {
	// Re-read the counter each tick: its watermark moves, and its token or
	// interval may have been changed through the bot or the API.
	counter, err := p.db.GetCounter(ctx, counterID)
	if err != nil {
		return err
	}
	client := NewLogsClient(counter, p.cfg.BaseURL)

	pending, err := p.db.PendingLogRequest(ctx, counterID)
	if err != nil {
		return err
	}
	if pending != nil {
		return p.advancePending(ctx, counter, client, pending)
	}
	return p.orderExport(ctx, counter, client)
}

// advancePending checks an in-flight export and consumes it once ready.
func (p *Poller) advancePending(ctx context.Context, counter *model.Counter, client *LogsClient, pending *model.LogRequest) error {
	req, err := client.Get(ctx, pending.RequestID)
	if err != nil {
		return fmt.Errorf("check export %d: %w", pending.RequestID, err)
	}

	if req.Status != pending.Status {
		if err := p.db.UpdateLogRequestStatus(ctx, pending.ID, req.Status); err != nil {
			return err
		}
	}

	switch {
	case req.Ready():
		return p.consumeExport(ctx, counter, client, pending, req)

	case req.Terminal():
		// Nothing will come of it. Drop the row so the next tick orders again
		// for the same window — no events are skipped.
		log.Printf("counter %s: export %d ended as %q, will re-order",
			counter.CounterID, req.RequestID, req.Status)
		return p.db.DeleteLogRequest(ctx, pending.ID)

	default:
		// Still created or awaiting_retry: preparation takes minutes.
		return nil
	}
}

// consumeExport downloads a processed export, evaluates it and advances the
// watermark.
func (p *Poller) consumeExport(ctx context.Context, counter *model.Counter, client *LogsClient, pending *model.LogRequest, req *LogRequest) error {
	since := time.Time{}
	if counter.LastEventAt != nil {
		since = *counter.LastEventAt
	}

	var events []*model.MetrikaEvent
	newest := since

	err := client.DownloadEvents(ctx, req, func(ev *model.MetrikaEvent) error {
		// The export window is inclusive at both ends, so its first events are
		// the ones the previous export already delivered.
		if !since.IsZero() && !ev.EventTime.After(since) {
			return nil
		}
		if ev.EventTime.After(newest) {
			newest = ev.EventTime
		}
		events = append(events, ev)
		return nil
	})
	if err != nil {
		return fmt.Errorf("download export %d: %w", req.RequestID, err)
	}

	if len(events) > 0 {
		if err := p.consumer(counter.ID, events); err != nil {
			// Leave the export in place: the next tick retries the download
			// rather than dropping a batch that was never evaluated.
			return fmt.Errorf("evaluate %d event(s): %w", len(events), err)
		}
	}

	// The window is fully processed even when it held nothing for us, so the
	// watermark advances to its end and the next export starts after it.
	watermark := pending.Date2
	if newest.After(watermark) {
		watermark = newest
	}
	if err := p.db.SetCounterWatermark(ctx, counter.ID, watermark); err != nil {
		return err
	}

	log.Printf("counter %s: export %d delivered %d event(s), window through %s",
		counter.CounterID, req.RequestID, len(events), watermark.Format(time.RFC3339))

	// Release the quota before dropping the row, so a failure here still leaves
	// something to reap.
	if err := client.Clean(ctx, req.RequestID); err != nil {
		log.Printf("counter %s: clean export %d: %v", counter.CounterID, req.RequestID, err)
	}
	return p.db.DeleteLogRequest(ctx, pending.ID)
}

// orderExport creates the next export for a counter, if the window is ripe.
func (p *Poller) orderExport(ctx context.Context, counter *model.Counter, client *LogsClient) error {
	window, ok := p.nextWindow(counter, time.Now())
	if !ok {
		// Not enough settled data has accumulated yet.
		return nil
	}

	// Asking first turns a quota rejection into one clear log line instead of
	// an export that is created and then fails.
	eval, err := client.Evaluate(ctx, window)
	if err != nil {
		return err
	}
	if !eval.Possible {
		return fmt.Errorf("Metrika will not export %s → %s (quota allows %d day(s))",
			window.From.Format(time.RFC3339), window.To.Format(time.RFC3339), eval.MaxPossibleDays)
	}

	req, err := client.Create(ctx, window)
	if err != nil {
		return err
	}

	record := &model.LogRequest{
		CounterID: counter.ID,
		RequestID: req.RequestID,
		Date1:     window.From,
		Date2:     window.To,
		Status:    req.Status,
	}
	if err := p.db.CreateLogRequest(ctx, record); err != nil {
		// The export exists at Metrika but is not tracked here. Cancel it now
		// rather than leaving quota held by something nothing will download.
		if cancelErr := client.Cancel(ctx, req.RequestID); cancelErr != nil {
			log.Printf("counter %s: could not cancel untracked export %d: %v",
				counter.CounterID, req.RequestID, cancelErr)
		}
		return err
	}

	log.Printf("counter %s: ordered export %d for %s → %s",
		counter.CounterID, req.RequestID,
		window.From.Format(time.RFC3339), window.To.Format(time.RFC3339))
	return nil
}

// nextWindow computes the period the next export should cover, or reports that
// it is too early to ask for one.
func (p *Poller) nextWindow(counter *model.Counter, now time.Time) (Window, bool) {
	// Everything up to this point is settled enough to export.
	end := now.Add(-p.cfg.lag())

	// Resume from the watermark however far back it sits. Clamping the start
	// forward instead would silently skip every event in between — a counter
	// idle for a month would lose the month.
	start := end.Add(-p.cfg.maxWindow())
	if counter.LastEventAt != nil {
		start = *counter.LastEventAt
	}

	if !end.After(start) {
		return Window{}, false
	}
	// Cap one export instead: a counter far behind is walked forward a window
	// per tick until it catches up.
	if end.Sub(start) > p.cfg.maxWindow() {
		end = start.Add(p.cfg.maxWindow())
	}
	return Window{From: start, To: end}, true
}

// RunOnce advances every counter a single step. Used by the /poll command and
// the REST API; because the API is asynchronous this usually orders an export
// rather than delivering events immediately.
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
