package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/isklv/metrika-alert/internal/engine"
	"github.com/isklv/metrika-alert/internal/model"
)

// Server exposes REST endpoints for managing counters, monitors, triggers, alerts.
type Server struct {
	db       *model.DB
	reporter *engine.Reporter
	poller   *engine.Poller
	mux      *http.ServeMux
	listen   string
}

func NewServer(db *model.DB, reporter *engine.Reporter, poller *engine.Poller, listenAddr string) *Server {
	s := &Server{db: db, reporter: reporter, poller: poller, listen: listenAddr, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/api/counters", s.handleCounters)
	s.mux.HandleFunc("/api/monitors", s.handleMonitors)
	s.mux.HandleFunc("/api/triggers", s.handleTriggers)
	s.mux.HandleFunc("/api/alert-actions", s.handleAlertActions)
	s.mux.HandleFunc("/api/alerts", s.handleAlerts)
	s.mux.HandleFunc("/api/reports", s.handleReports)
	s.mux.HandleFunc("/api/events", s.handleEvents)
	s.mux.HandleFunc("/api/status", s.handleStatus)
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	server := &http.Server{
		Addr:    s.listen,
		Handler: s.mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()

	log.Printf("api server listening on %s", s.listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("api server: %w", err)
	}
	return nil
}

// ---- handlers ----

func (s *Server) handleCounters(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		data, err := s.db.ListCounters(r.Context())
		s.getJSON(w, data, err)
	case http.MethodPost:
		s.createCounter(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) createCounter(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name            string `json:"name"`
		CounterID       string `json:"counter_id"`
		OAuthToken      string `json:"oauth_token"`
		PollIntervalMin int    `json:"poll_interval_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Name == "" || body.CounterID == "" || body.OAuthToken == "" {
		http.Error(w, "name, counter_id, oauth_token required", http.StatusBadRequest)
		return
	}
	if body.PollIntervalMin <= 0 {
		body.PollIntervalMin = 60
	}

	c := &model.Counter{
		Name:         body.Name,
		CounterID:    body.CounterID,
		OAuthToken:   body.OAuthToken,
		PollInterval: body.PollIntervalMin,
	}
	if err := s.db.CreateCounter(r.Context(), c); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(c)
}

func (s *Server) handleMonitors(w http.ResponseWriter, r *http.Request) {
	counterID := parseOptionalInt64(r.URL.Query().Get("counter_id"))
	switch r.Method {
	case http.MethodGet:
		if counterID <= 0 {
			http.Error(w, "counter_id required", http.StatusBadRequest)
			return
		}
		data, err := s.db.ListMonitors(r.Context(), counterID)
		s.getJSON(w, data, err)
	case http.MethodPost:
		if counterID <= 0 {
			counterID = parseBodyInt64(r, "counter_id")
		}
		s.createMonitor(w, r, counterID)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) createMonitor(w http.ResponseWriter, r *http.Request, counterID int64) {
	var body struct {
		Name       string   `json:"name"`
		URLPattern string   `json:"url_pattern"`
		Metrics    []string `json:"metrics"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Name == "" || body.URLPattern == "" || counterID <= 0 {
		http.Error(w, "counter_id, name, url_pattern required", http.StatusBadRequest)
		return
	}
	if len(body.Metrics) == 0 {
		body.Metrics = []string{"visits", "bounces", "goals", "revenue"}
	}

	m := &model.PageMonitor{
		CounterID:  counterID,
		Name:       body.Name,
		URLPattern: body.URLPattern,
		Metrics:    body.Metrics,
		Enabled:    true,
	}
	if err := s.db.CreateMonitor(r.Context(), m); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(m)
}

func (s *Server) handleTriggers(w http.ResponseWriter, r *http.Request) {
	counterID := parseOptionalInt64(r.URL.Query().Get("counter_id"))
	switch r.Method {
	case http.MethodGet:
		if counterID <= 0 {
			http.Error(w, "counter_id required", http.StatusBadRequest)
			return
		}
		data, err := s.db.ListTriggers(r.Context(), counterID)
		s.getJSON(w, data, err)
	case http.MethodPost:
		if counterID <= 0 {
			counterID = parseBodyInt64(r, "counter_id")
		}
		s.createTrigger(w, r, counterID)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) createTrigger(w http.ResponseWriter, r *http.Request, counterID int64) {
	var body struct {
		Name           string `json:"name"`
		MonitorID      *int64 `json:"monitor_id,omitempty"`
		Condition      string `json:"condition"`
		Threshold      int    `json:"threshold"`
		WindowMinutes  int    `json:"window_minutes"`
		CooldownMinutes int   `json:"cooldown_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Name == "" || body.Condition == "" || counterID <= 0 {
		http.Error(w, "counter_id, name, condition required", http.StatusBadRequest)
		return
	}
	if body.Threshold <= 0 {
		body.Threshold = 1
	}
	if body.WindowMinutes <= 0 {
		body.WindowMinutes = 30
	}
	if body.CooldownMinutes <= 0 {
		body.CooldownMinutes = 60
	}

	t := &model.Trigger{
		CounterID: counterID, MonitorID: body.MonitorID, Name: body.Name,
		Condition: body.Condition, Threshold: body.Threshold,
		Window: body.WindowMinutes, Cooldown: body.CooldownMinutes, Enabled: true,
	}
	if err := s.db.CreateTrigger(r.Context(), t); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(t)
}

func (s *Server) handleAlertActions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		data, err := s.db.ListAlertActions(r.Context())
		s.getJSON(w, data, err)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	counterID := parseOptionalInt64(r.URL.Query().Get("counter_id"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	data, err := s.db.RecentAlerts(r.Context(), counterID, limit)
	s.getJSON(w, data, err)
}

func (s *Server) handleReports(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		counterID := parseOptionalInt64(r.URL.Query().Get("counter_id"))
		period := r.URL.Query().Get("period")
		if period == "" {
			period = "hour"
		}
		if counterID <= 0 {
			http.Error(w, "counter_id required", http.StatusBadRequest)
			return
		}
		data, err := s.db.ListSnapshots(r.Context(), counterID, period, 20)
		s.getJSON(w, data, err)
	case http.MethodPost:
		counterID := parseOptionalInt64(r.URL.Query().Get("counter_id"))
		if err := s.runReportForCounter(r.Context(), counterID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, "report queued")
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) runReportForCounter(ctx context.Context, counterID int64) error {
	if counterID > 0 {
		counter, err := s.db.GetCounter(ctx, counterID)
		if err != nil {
			return err
		}
		client := engine.NewMetrikaClient(counter, "", "")
		return s.reporter.ReportCounter(ctx, counter, client, time.Now())
	}
	return s.reporter.RunReports(ctx)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	counterID := parseOptionalInt64(r.URL.Query().Get("counter_id"))
	if counterID <= 0 {
		http.Error(w, "counter_id required", http.StatusBadRequest)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 100
	}

	type eventOut struct {
		EventTime string `json:"event_time"`
		PageURL   string `json:"page_url"`
		Title     string `json:"title"`
		GoalsID   []string `json:"goals_id"`
		Status    string `json:"status_code"`
		Revenue   float64 `json:"revenue"`
	}

	alerts, err := s.db.RecentAlerts(r.Context(), counterID, 0)
	if err == nil {
		// Events are not persisted individually — return empty with note.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"note":   "raw events are streamed, not persisted. Use /api/reports for aggregated data.",
			"alerts": len(alerts),
		})
		return
	}
	s.getJSON(w, nil, err)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	counters, _ := s.db.ListCounters(r.Context())
	alerts, _ := s.db.RecentAlerts(r.Context(), 0, 1)
	lastAlert := "never"
	if len(alerts) > 0 {
		lastAlert = alerts[0].CreatedAt.Format(time.RFC3339)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":     "running",
		"counters":   len(counters),
		"last_alert": lastAlert,
	})
}

// ---- helpers ----

func (s *Server) getJSON(w http.ResponseWriter, data any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if data == nil {
		data = []any{}
	}
	json.NewEncoder(w).Encode(data)
}

func parseOptionalInt64(s string) int64 {
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return v
}

func parseBodyInt64(r *http.Request, key string) int64 {
	// Read and buffer the body so the caller can re-decode it afterwards.
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return 0
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return 0
	}
	var v int64
	if rawv, ok := body[key]; ok {
		json.Unmarshal(rawv, &v)
	}
	return v
}
