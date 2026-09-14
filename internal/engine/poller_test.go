package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

// metrikaFake is a stateful stand-in for the Logs API: it moves an export
// through created → processed the way the real service does.
type metrikaFake struct {
	mu sync.Mutex

	nextID    int64
	status    string
	created   []Window
	cleaned   []int64
	cancelled []int64
	tsv       string
	evaluable bool
}

func newMetrikaFake() *metrikaFake {
	return &metrikaFake{nextID: 100, status: StatusCreated, evaluable: true, tsv: ""}
}

func (f *metrikaFake) setStatus(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = s
}

func (f *metrikaFake) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := r.URL.Path

		switch {
		case strings.HasSuffix(path, "/logrequests/evaluate"):
			fmt.Fprintf(w, `{"log_request_evaluation":{"possible":%t,"max_possible_day_quantity":7}}`, f.evaluable)

		case strings.HasSuffix(path, "/logrequests") && r.Method == http.MethodPost:
			q := r.URL.Query()
			from, _ := time.Parse(logsDateLayout, q.Get("date1"))
			to, _ := time.Parse(logsDateLayout, q.Get("date2"))
			f.created = append(f.created, Window{From: from, To: to})
			fmt.Fprintf(w, `{"log_request":{"request_id":%d,"status":"created"}}`, f.nextID)

		case strings.HasSuffix(path, "/clean"):
			f.cleaned = append(f.cleaned, f.nextID)
			fmt.Fprint(w, `{"log_request":{"status":"cleaned_by_user"}}`)

		case strings.HasSuffix(path, "/cancel"):
			f.cancelled = append(f.cancelled, f.nextID)
			fmt.Fprint(w, `{"log_request":{"status":"canceled"}}`)

		case strings.Contains(path, "/part/"):
			fmt.Fprint(w, f.tsv)

		default: // status check
			parts := ""
			if f.status == StatusProcessed {
				parts = `,"parts":[{"part_number":0,"size":100}]`
			}
			fmt.Fprintf(w, `{"log_request":{"request_id":%d,"status":%q%s}}`, f.nextID, f.status, parts)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

type pollerHarness struct {
	db     *model.DB
	poller *Poller
	fake   *metrikaFake
	events [][]*model.MetrikaEvent
	mu     sync.Mutex
}

func newPollerHarness(t *testing.T) *pollerHarness {
	t.Helper()
	db, err := model.OpenDB(filepath.Join(t.TempDir(), "poll.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := db.CreateCounter(context.Background(), testCounter()); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}

	h := &pollerHarness{db: db, fake: newMetrikaFake()}
	cfg := &MetrikaConfig{BaseURL: h.fake.serve(t), LagHours: 26, MaxWindowHours: 24}
	h.poller = NewPoller(db, cfg, func(counterID int64, events []*model.MetrikaEvent) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.events = append(h.events, events)
		return nil
	})
	return h
}

func (h *pollerHarness) advance(t *testing.T) {
	t.Helper()
	if err := h.poller.advance(context.Background(), 1); err != nil {
		t.Fatalf("advance: %v", err)
	}
}

func (h *pollerHarness) delivered() []*model.MetrikaEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*model.MetrikaEvent
	for _, batch := range h.events {
		out = append(out, batch...)
	}
	return out
}

func (h *pollerHarness) counter(t *testing.T) *model.Counter {
	t.Helper()
	c, err := h.db.GetCounter(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetCounter: %v", err)
	}
	return c
}

// The whole lifecycle: order, wait, download, evaluate, clean.
func TestPollerDrivesExportLifecycle(t *testing.T) {
	h := newPollerHarness(t)
	ctx := context.Background()

	// Tick 1 — nothing in flight, so an export is ordered and tracked.
	h.advance(t)
	if len(h.fake.created) != 1 {
		t.Fatalf("ordered %d exports, want 1", len(h.fake.created))
	}
	pending, err := h.db.PendingLogRequest(ctx, 1)
	if err != nil || pending == nil {
		t.Fatalf("export was not recorded: %v", err)
	}
	if len(h.delivered()) != 0 {
		t.Error("events were delivered before the export was ready")
	}

	// Tick 2 — still preparing. No second export may be ordered.
	h.advance(t)
	if len(h.fake.created) != 1 {
		t.Fatalf("ordered %d exports while one was pending", len(h.fake.created))
	}

	// Tick 3 — ready, so it is downloaded and evaluated.
	h.fake.tsv = "ym:pv:dateTime\tym:pv:URL\tym:pv:httpError\n" +
		"2026-09-10 10:00:00\t/checkout\t500\n" +
		"2026-09-10 10:05:00\t/cart\t200\n"
	h.fake.setStatus(StatusProcessed)
	h.advance(t)

	events := h.delivered()
	if len(events) != 2 {
		t.Fatalf("delivered %d events, want 2", len(events))
	}
	if events[0].PageURL != "/checkout" || events[0].Status != "500" {
		t.Errorf("first event = %+v", events[0])
	}

	// The export must be released: Metrika caps prepared requests per counter.
	if len(h.fake.cleaned) != 1 {
		t.Errorf("cleaned %d exports, want 1", len(h.fake.cleaned))
	}
	if pending, _ := h.db.PendingLogRequest(ctx, 1); pending != nil {
		t.Error("a consumed export is still tracked as pending")
	}
	if h.counter(t).LastEventAt == nil {
		t.Error("watermark was not advanced")
	}
}

// The watermark has to survive a restart, or every restart replays a day of
// events and re-fires every trigger.
func TestWatermarkPersistsAcrossRestart(t *testing.T) {
	h := newPollerHarness(t)

	h.advance(t)
	h.fake.tsv = "ym:pv:dateTime\tym:pv:URL\n2026-09-10 10:00:00\t/a\n"
	h.fake.setStatus(StatusProcessed)
	h.advance(t)

	first := h.counter(t).LastEventAt
	if first == nil {
		t.Fatal("watermark not set")
	}

	// A fresh Poller over the same database is what a restart looks like.
	restarted := NewPoller(h.db, &MetrikaConfig{BaseURL: h.fake.serve(t)}, func(int64, []*model.MetrikaEvent) error {
		t.Error("a restart replayed events that were already evaluated")
		return nil
	})
	counter := h.counter(t)
	window, ok := restarted.nextWindow(counter, time.Now())
	if !ok {
		t.Fatal("no window after restart")
	}
	if !window.From.Equal(*first) {
		t.Errorf("window starts at %s, want the stored watermark %s", window.From, *first)
	}
}

// Events at or before the watermark were already evaluated; re-delivering them
// would re-fire every trigger.
func TestPollerSkipsEventsAtOrBeforeWatermark(t *testing.T) {
	h := newPollerHarness(t)
	ctx := context.Background()

	cutoff := time.Date(2026, 9, 10, 10, 0, 0, 0, time.Local)
	if err := h.db.SetCounterWatermark(ctx, 1, cutoff); err != nil {
		t.Fatalf("SetCounterWatermark: %v", err)
	}

	h.advance(t)
	h.fake.tsv = "ym:pv:dateTime\tym:pv:URL\n" +
		"2026-09-10 09:59:00\t/before\n" +
		"2026-09-10 10:00:00\t/exactly-at\n" +
		"2026-09-10 10:01:00\t/after\n"
	h.fake.setStatus(StatusProcessed)
	h.advance(t)

	events := h.delivered()
	if len(events) != 1 {
		t.Fatalf("delivered %d events, want only the one after the watermark: %+v", len(events), events)
	}
	if events[0].PageURL != "/after" {
		t.Errorf("delivered %q", events[0].PageURL)
	}
}

// A failed export must not wedge the counter forever.
func TestPollerReordersAfterTerminalStatus(t *testing.T) {
	h := newPollerHarness(t)

	h.advance(t)
	h.fake.setStatus(StatusFailed)
	h.advance(t) // notices the failure and drops the tracking row

	if pending, _ := h.db.PendingLogRequest(context.Background(), 1); pending != nil {
		t.Fatal("a failed export is still tracked")
	}

	h.fake.setStatus(StatusCreated)
	h.advance(t)
	if len(h.fake.created) != 2 {
		t.Errorf("ordered %d exports, want a replacement for the failed one", len(h.fake.created))
	}
}

// A consumer failure must not advance the watermark past events that were
// never evaluated.
func TestPollerKeepsExportWhenEvaluationFails(t *testing.T) {
	h := newPollerHarness(t)
	ctx := context.Background()

	h.poller.consumer = func(int64, []*model.MetrikaEvent) error {
		return fmt.Errorf("evaluator down")
	}

	h.advance(t)
	h.fake.tsv = "ym:pv:dateTime\tym:pv:URL\n2026-09-10 10:00:00\t/a\n"
	h.fake.setStatus(StatusProcessed)

	if err := h.poller.advance(ctx, 1); err == nil {
		t.Fatal("expected the consumer failure to surface")
	}
	if h.counter(t).LastEventAt != nil {
		t.Error("watermark advanced past events that were never evaluated")
	}
	if pending, _ := h.db.PendingLogRequest(ctx, 1); pending == nil {
		t.Error("the export was dropped, so the batch can never be retried")
	}
}

// Metrika refusing the window on quota should be reported, not retried blindly.
func TestPollerReportsQuotaRefusal(t *testing.T) {
	h := newPollerHarness(t)
	h.fake.evaluable = false

	err := h.poller.advance(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("err = %v, want a quota refusal", err)
	}
	if len(h.fake.created) != 0 {
		t.Error("an export was ordered despite the refusal")
	}
}

// A window is only ordered once enough settled data exists behind it.
func TestNextWindowRespectsTheAPILag(t *testing.T) {
	p := NewPoller(nil, &MetrikaConfig{LagHours: 26, MaxWindowHours: 24}, nil)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

	t.Run("fresh counter looks back one window", func(t *testing.T) {
		w, ok := p.nextWindow(&model.Counter{}, now)
		if !ok {
			t.Fatal("no window for a fresh counter")
		}
		// The Logs API refuses a window ending today, so the end must trail now.
		if !w.To.Before(now.Add(-24 * time.Hour)) {
			t.Errorf("window ends at %s, too close to %s", w.To, now)
		}
		if w.To.Sub(w.From) > 24*time.Hour {
			t.Errorf("window spans %s, above the cap", w.To.Sub(w.From))
		}
	})

	t.Run("waits when the watermark is already current", func(t *testing.T) {
		recent := now.Add(-time.Hour)
		if _, ok := p.nextWindow(&model.Counter{LastEventAt: &recent}, now); ok {
			t.Error("ordered an export for a window that is not settled yet")
		}
	})

	t.Run("catch-up is capped", func(t *testing.T) {
		old := now.AddDate(0, 0, -30)
		w, ok := p.nextWindow(&model.Counter{LastEventAt: &old}, now)
		if !ok {
			t.Fatal("no window for a counter that is far behind")
		}
		if !w.From.Equal(old) {
			t.Errorf("window starts at %s, want the watermark %s", w.From, old)
		}
		if w.To.Sub(w.From) > 24*time.Hour {
			t.Errorf("catch-up window spans %s, want it capped at 24h", w.To.Sub(w.From))
		}
	})
}

// An export orphaned by a killed process holds quota that new ones need.
func TestStaleExportsAreReaped(t *testing.T) {
	h := newPollerHarness(t)
	ctx := context.Background()

	record := &model.LogRequest{
		CounterID: 1, RequestID: 100, Status: StatusCreated,
		Date1: time.Now().Add(-48 * time.Hour), Date2: time.Now().Add(-24 * time.Hour),
	}
	if err := h.db.CreateLogRequest(ctx, record); err != nil {
		t.Fatalf("CreateLogRequest: %v", err)
	}
	// Backdate it past the staleness threshold.
	if _, err := h.db.ExecContext(ctx,
		`UPDATE log_requests SET updated_at = ? WHERE id = ?`, time.Now().Add(-24*time.Hour), record.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	counters, _ := h.db.ListCounters(ctx)
	h.poller.reapStaleRequests(ctx, counters)

	if pending, _ := h.db.PendingLogRequest(ctx, 1); pending != nil {
		t.Error("stale export is still tracked")
	}
	if len(h.fake.cancelled) != 1 {
		t.Errorf("cancelled %d exports, want 1", len(h.fake.cancelled))
	}
}

func TestMetrikaConfigDefaults(t *testing.T) {
	cfg := &MetrikaConfig{}
	if cfg.lag() != DefaultLagHours*time.Hour {
		t.Errorf("lag = %s", cfg.lag())
	}
	if cfg.maxWindow() != DefaultMaxWindowHours*time.Hour {
		t.Errorf("maxWindow = %s", cfg.maxWindow())
	}
	// The Logs API refuses date2 = today, so the lag cannot sit under a day.
	if cfg.lag() < 24*time.Hour {
		t.Errorf("default lag %s is below the API's own floor", cfg.lag())
	}
}

// Walking forward a window at a time must actually reach the present rather
// than stalling short of it.
func TestCatchUpConvergesOnThePresent(t *testing.T) {
	p := NewPoller(nil, &MetrikaConfig{LagHours: 26, MaxWindowHours: 24}, nil)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

	watermark := now.AddDate(0, 0, -10)
	counter := &model.Counter{LastEventAt: &watermark}

	steps := 0
	for {
		w, ok := p.nextWindow(counter, now)
		if !ok {
			break
		}
		if !w.From.Equal(*counter.LastEventAt) {
			t.Fatalf("step %d starts at %s, skipping past the watermark %s", steps, w.From, *counter.LastEventAt)
		}
		end := w.To
		counter.LastEventAt = &end
		steps++
		if steps > 20 {
			t.Fatal("catch-up did not converge")
		}
	}

	// The backlog runs from the watermark to now minus the lag — 214 hours —
	// so 24-hour steps clear it in nine.
	if steps != 9 {
		t.Errorf("took %d steps to catch up, want 9", steps)
	}
	if got := now.Sub(*counter.LastEventAt); got > 27*time.Hour {
		t.Errorf("stalled %s behind now, want it within the lag", got)
	}
}
