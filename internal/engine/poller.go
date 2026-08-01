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

// Poller periodically fetches events from the Logs API for each counter.
type Poller struct {
	db       *model.DB
	cfg      *MetrikaConfig
	consumer EventConsumer
	running  bool
	mu       sync.Mutex
	stopped  chan struct{}
}

type MetrikaConfig struct {
	BaseURL string
	LogsURL string
}

func NewPoller(db *model.DB, cfg *MetrikaConfig, consumer EventConsumer) *Poller {
	return &Poller{db: db, cfg: cfg, consumer: consumer, stopped: make(chan struct{})}
}

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
		log.Printf("poller started: no counters configured")
		close(p.stopped)
		return nil
	}

	log.Printf("poller started: %d counter(s)", len(counters))

	for _, c := range counters {
		go p.pollCounterLoop(ctx, c)
	}

	// Wait for context cancellation (graceful shutdown).
	<-ctx.Done()
	p.mu.Lock()
	p.running = false
	p.mu.Unlock()
	close(p.stopped)
	return ctx.Err()
}

func (p *Poller) Stop() {
	<-p.stopped
}

// pollCounterLoop polls one counter at its configured interval.
func (p *Poller) pollCounterLoop(ctx context.Context, c model.Counter) {
	client := NewMetrikaClient(&c, p.cfg.BaseURL, p.cfg.LogsURL)
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
			p.pollOnce(ctx, c.ID, client)
		}
	}
}

func (p *Poller) pollOnce(ctx context.Context, counterID int64, client *MetrikaClient) {
	hits, err := client.FetchEvents(ctx, time.Time{}, 5000)
	if err != nil {
		log.Printf("poll counter %d: %v", counterID, err)
		return
	}
	if len(hits) == 0 {
		return
	}

	events := make([]*model.MetrikaEvent, 0, len(hits))
	for _, hit := range hits {
		events = append(events, ParseEvent(hit))
	}

	if err := p.consumer(counterID, events); err != nil {
		log.Printf("process events for counter %d: %v", counterID, err)
	}
}

// RunOnce polls all counters a single time. Used by manual triggers (bot /api).
func (p *Poller) RunOnce(ctx context.Context) error {
	counters, err := p.db.ListCounters(ctx)
	if err != nil {
		return fmt.Errorf("list counters: %w", err)
	}

	for _, c := range counters {
		client := NewMetrikaClient(&c, p.cfg.BaseURL, p.cfg.LogsURL)
		if err := p.pollOnceSync(ctx, c.ID, client); err != nil {
			log.Printf("run-once counter %d: %v", c.ID, err)
		}
	}
	return nil
}

func (p *Poller) pollOnceSync(ctx context.Context, counterID int64, client *MetrikaClient) error {
	hits, err := client.FetchEvents(ctx, time.Time{}, 5000)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		return nil
	}

	events := make([]*model.MetrikaEvent, 0, len(hits))
	for _, hit := range hits {
		events = append(events, ParseEvent(hit))
	}
	return p.consumer(counterID, events)
}
