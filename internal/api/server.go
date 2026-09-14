package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/isklv/metrika-alert/internal/engine"
	"github.com/isklv/metrika-alert/internal/model"
)

// Server exposes REST endpoints for managing counters, monitors, triggers, alerts.
type Server struct {
	db        *model.DB
	reporter  *engine.Reporter
	poller    *engine.Poller
	metrika   *engine.MetrikaConfig
	mux       *http.ServeMux
	listen    string
	authToken string
}

// Config wires the API server. AuthToken, when set, is required as a bearer
// token on every request.
type Config struct {
	ListenAddr string
	AuthToken  string
	Metrika    *engine.MetrikaConfig
}

func NewServer(db *model.DB, reporter *engine.Reporter, poller *engine.Poller, cfg Config) *Server {
	s := &Server{
		db:        db,
		reporter:  reporter,
		poller:    poller,
		metrika:   cfg.Metrika,
		listen:    cfg.ListenAddr,
		authToken: cfg.AuthToken,
		mux:       http.NewServeMux(),
	}
	s.routes()
	return s
}

// authorized reports whether a request may proceed. With no token configured
// the API is open, which is only safe on a loopback or private listen address —
// the deployment guide says so, and startup logs a warning.
func (s *Server) authorized(r *http.Request) bool {
	if s.authToken == "" {
		return true
	}
	header := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return false
	}
	// Constant-time compare keeps the token from leaking through timing.
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.authToken)) == 1
}

// withAuth wraps a handler with the bearer-token check.
func (s *Server) withAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("/api/counters", s.withAuth(s.handleCounters))
	s.mux.HandleFunc("/api/triggers", s.withAuth(s.handleTriggers))
	s.mux.HandleFunc("/api/alert-actions", s.withAuth(s.handleAlertActions))
	s.mux.HandleFunc("/api/alerts", s.withAuth(s.handleAlerts))
	s.mux.HandleFunc("/api/reports", s.withAuth(s.handleReports))
	s.mux.HandleFunc("/api/status", s.withAuth(s.handleStatus))
	// Liveness probe: unauthenticated on purpose, and reveals nothing.
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	server := &http.Server{
		Addr:              s.listen,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()

	if s.authToken == "" {
		log.Printf("api server: WARNING — no auth_token set; anyone who can reach %s can read and write counters", s.listen)
	}
	log.Printf("api server listening on %s", s.listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("api server: %w", err)
	}
	return nil
}

// Defaults applied to a rule created through the API, matching the bot's.
const (
	defaultMinBaseline     = 10
	defaultBaselineWeeks   = 4
	defaultCooldownMinutes = 180
)

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
		body, err := readBody(r)
		if err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if counterID <= 0 {
			counterID = bodyInt64(body, "counter_id")
		}
		s.createTrigger(w, r, body, counterID)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) createTrigger(w http.ResponseWriter, r *http.Request, raw []byte, counterID int64) {
	var body struct {
		Name          string `json:"name"`
		Metric        string `json:"metric"`
		Direction     string `json:"direction"`
		DeviationPct  int    `json:"deviation_percent"`
		MinBaseline   *int   `json:"min_baseline,omitempty"`
		BaselineWeeks int    `json:"baseline_weeks"`
		Cooldown      int    `json:"cooldown_minutes"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Name == "" || counterID <= 0 {
		http.Error(w, "counter_id and name required", http.StatusBadRequest)
		return
	}
	if _, err := engine.ResolveMetric(body.Metric); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Direction == "" {
		body.Direction = engine.DirectionDrop
	}
	if !engine.ValidDirection(body.Direction) {
		http.Error(w, "direction must be drop, rise or both", http.StatusBadRequest)
		return
	}
	if body.DeviationPct <= 0 || body.DeviationPct > 100 {
		http.Error(w, "deviation_percent must be between 1 and 100", http.StatusBadRequest)
		return
	}
	if body.BaselineWeeks <= 0 {
		body.BaselineWeeks = defaultBaselineWeeks
	}
	if body.Cooldown <= 0 {
		body.Cooldown = defaultCooldownMinutes
	}
	// A zero floor is a deliberate choice, so only an absent field defaults.
	minBaseline := defaultMinBaseline
	if body.MinBaseline != nil {
		minBaseline = *body.MinBaseline
	}

	t := &model.Trigger{
		CounterID:     counterID,
		Name:          body.Name,
		Metric:        strings.ToLower(body.Metric),
		Direction:     body.Direction,
		DeviationPct:  body.DeviationPct,
		MinBaseline:   minBaseline,
		BaselineWeeks: body.BaselineWeeks,
		Cooldown:      body.Cooldown,
		Enabled:       true,
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
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "report delivered"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) runReportForCounter(ctx context.Context, counterID int64) error {
	if counterID > 0 {
		return s.reporter.RunReportFor(ctx, counterID)
	}
	return s.reporter.RunReports(ctx)
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
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// An empty result is a nil slice, which marshals to `null`. A list endpoint
	// should answer with an empty array so clients can iterate unconditionally.
	if v := reflect.ValueOf(data); !v.IsValid() || (v.Kind() == reflect.Slice && v.IsNil()) {
		data = []any{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func parseOptionalInt64(s string) int64 {
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return v
}

// maxBodyBytes caps a request body; the payloads here are a few hundred bytes.
const maxBodyBytes = 1 << 20

// readBody buffers the request body so it can be decoded more than once. The
// previous helpers each decoded straight from r.Body, so the second one always
// saw an already-drained reader and failed with EOF.
func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
}

// bodyInt64 pulls one integer field out of a buffered JSON body.
func bodyInt64(raw []byte, key string) int64 {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return 0
	}
	var v int64
	if field, ok := body[key]; ok {
		json.Unmarshal(field, &v)
	}
	return v
}
