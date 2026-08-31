package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

func openEngineDB(t *testing.T) *model.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := model.OpenDB(filepath.Join(dir, "poller.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestPollerRunOnce_DeliversEvents(t *testing.T) {
	db := openEngineDB(t)
	ctx := context.Background()

	c := &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_hits":2,"hits":[{"event_time":"2026-08-01T10:00:00Z","page_url":"/a","status_code":500},{"event_time":"2026-08-01T10:01:00Z","page_url":"/b","status_code":200}]}`))
	}))
	t.Cleanup(srv.Close)

	var gotCounter int64
	var gotEvents atomic.Int64
	consumer := func(counterID int64, events []*model.MetrikaEvent) error {
		gotCounter = counterID
		gotEvents.Store(int64(len(events)))
		return nil
	}

	p := NewPoller(db, &MetrikaConfig{BaseURL: srv.URL, LogsURL: srv.URL}, consumer)
	if err := p.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if gotCounter != c.ID {
		t.Errorf("consumer counter = %d, want %d", gotCounter, c.ID)
	}
	if gotEvents.Load() != 2 {
		t.Errorf("consumer events = %d, want 2", gotEvents.Load())
	}
}

func TestPollerRunOnce_NoCounters(t *testing.T) {
	db := openEngineDB(t)
	called := false
	p := NewPoller(db, &MetrikaConfig{BaseURL: "http://unused"}, func(int64, []*model.MetrikaEvent) error {
		called = true
		return nil
	})
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce (no counters): %v", err)
	}
	if called {
		t.Error("consumer should not be called with no counters")
	}
}

func TestPollerRunOnce_FetchErrorDoesNotFail(t *testing.T) {
	db := openEngineDB(t)
	ctx := context.Background()
	c := &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":401,"message":"bad token"}}`))
	}))
	t.Cleanup(srv.Close)

	p := NewPoller(db, &MetrikaConfig{BaseURL: srv.URL, LogsURL: srv.URL}, func(int64, []*model.MetrikaEvent) error {
		return nil
	})
	// RunOnce swallows per-counter errors and returns nil.
	if err := p.RunOnce(ctx); err != nil {
		t.Errorf("RunOnce should not fail on fetch error, got %v", err)
	}
}

func TestPollerPollOnceSync_EmptyHits(t *testing.T) {
	db := openEngineDB(t)
	ctx := context.Background()
	c := &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_hits":0,"hits":[]}`))
	}))
	t.Cleanup(srv.Close)

	called := false
	p := NewPoller(db, &MetrikaConfig{BaseURL: srv.URL, LogsURL: srv.URL}, func(int64, []*model.MetrikaEvent) error {
		called = true
		return nil
	})
	client := NewMetrikaClient(c, srv.URL, srv.URL)
	if err := p.pollOnceSync(ctx, c.ID, client); err != nil {
		t.Fatalf("pollOnceSync: %v", err)
	}
	if called {
		t.Error("consumer should not be called for empty hits")
	}
}

func TestPollerPollOnceSync_ConsumerError(t *testing.T) {
	db := openEngineDB(t)
	ctx := context.Background()
	c := &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_hits":1,"hits":[{"page_url":"/a"}]}`))
	}))
	t.Cleanup(srv.Close)

	wantErr := errSentinel("boom")
	p := NewPoller(db, &MetrikaConfig{BaseURL: srv.URL, LogsURL: srv.URL}, func(int64, []*model.MetrikaEvent) error {
		return wantErr
	})
	client := NewMetrikaClient(c, srv.URL, srv.URL)
	if err := p.pollOnceSync(ctx, c.ID, client); err != wantErr {
		t.Errorf("pollOnceSync = %v, want %v", err, wantErr)
	}
}

func TestPollerStart_NoCounters(t *testing.T) {
	db := openEngineDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := NewPoller(db, &MetrikaConfig{BaseURL: "http://unused"}, func(int64, []*model.MetrikaEvent) error {
		return nil
	})
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start (no counters): %v", err)
	}
	// Stop should unblock immediately since stopped was closed.
	done := make(chan struct{})
	go func() { p.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked after no-counter Start")
	}
}

func TestPollerStart_AlreadyRunning(t *testing.T) {
	db := openEngineDB(t)
	p := NewPoller(db, &MetrikaConfig{BaseURL: "http://unused"}, func(int64, []*model.MetrikaEvent) error {
		return nil
	})
	// Simulate an in-flight Start (running flag set, stopped not yet closed).
	p.mu.Lock()
	p.running = true
	p.mu.Unlock()

	if err := p.Start(context.Background()); err == nil {
		t.Fatal("expected 'already running' error on second Start")
	}
}

func TestPollerStart_CancelsOnContext(t *testing.T) {
	db := openEngineDB(t)
	ctx, cancel := context.WithCancel(context.Background())

	c := &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	p := NewPoller(db, &MetrikaConfig{BaseURL: "http://unused"}, func(int64, []*model.MetrikaEvent) error {
		return nil
	})

	started := make(chan error, 1)
	go func() { started <- p.Start(ctx) }()

	// Give the loop a moment to register, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-started:
		if err != context.Canceled {
			t.Errorf("Start returned %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
	// After Start returns, stopped is closed so Stop unblocks.
	done := make(chan struct{})
	go func() { p.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked after Start returned")
	}
}

// errSentinel is a simple comparable error for assertions.
type errSentinel string

func (e errSentinel) Error() string { return string(e) }

func TestPollerPollOnce_Delivers(t *testing.T) {
	db := openEngineDB(t)
	ctx := context.Background()
	c := &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_hits":2,"hits":[{"page_url":"/a"},{"page_url":"/b"}]}`))
	}))
	t.Cleanup(srv.Close)

	var gotCounter int64
	var gotEvents atomic.Int64
	p := NewPoller(db, &MetrikaConfig{BaseURL: srv.URL, LogsURL: srv.URL}, func(counterID int64, events []*model.MetrikaEvent) error {
		gotCounter = counterID
		gotEvents.Store(int64(len(events)))
		return nil
	})
	client := NewMetrikaClient(c, srv.URL, srv.URL)
	// pollOnce is the async variant used by the ticker loop; it swallows
	// consumer errors and never returns one.
	p.pollOnce(ctx, c.ID, client)
	if gotCounter != c.ID {
		t.Errorf("consumer counter = %d, want %d", gotCounter, c.ID)
	}
	if gotEvents.Load() != 2 {
		t.Errorf("consumer events = %d, want 2", gotEvents.Load())
	}
}

func TestPollerPollOnce_FetchError(t *testing.T) {
	db := openEngineDB(t)
	ctx := context.Background()
	c := &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":401,"message":"bad token"}}`))
	}))
	t.Cleanup(srv.Close)

	called := false
	p := NewPoller(db, &MetrikaConfig{BaseURL: srv.URL, LogsURL: srv.URL}, func(int64, []*model.MetrikaEvent) error {
		called = true
		return nil
	})
	client := NewMetrikaClient(c, srv.URL, srv.URL)
	// Fetch error is logged and swallowed; consumer not called.
	p.pollOnce(ctx, c.ID, client)
	if called {
		t.Error("consumer should not be called on fetch error")
	}
}

func TestPollerPollOnce_ConsumerError(t *testing.T) {
	db := openEngineDB(t)
	ctx := context.Background()
	c := &model.Counter{Name: "Shop", CounterID: "12345", OAuthToken: "tok", PollInterval: 60}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("create counter: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_hits":1,"hits":[{"page_url":"/a"}]}`))
	}))
	t.Cleanup(srv.Close)

	// Consumer error is logged and swallowed by pollOnce (no panic, no return).
	p := NewPoller(db, &MetrikaConfig{BaseURL: srv.URL, LogsURL: srv.URL}, func(int64, []*model.MetrikaEvent) error {
		return errSentinel("consumer boom")
	})
	client := NewMetrikaClient(c, srv.URL, srv.URL)
	p.pollOnce(ctx, c.ID, client)
}
