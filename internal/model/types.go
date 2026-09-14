package model

import (
	"strconv"
	"time"
)

// Counter represents a Yandex.Metrika counter being monitored.
type Counter struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	CounterID string `json:"counter_id"`
	// The token is never serialised: counters are returned by the REST API,
	// and a GET /api/counters must not hand out Metrika credentials.
	OAuthToken   string `json:"-"`
	PollInterval int    `json:"poll_interval_minutes"` // how often to poll Logs API, minutes
	// LastEventAt is the newest event already evaluated. Nil on a counter that
	// has never been polled.
	LastEventAt *time.Time `json:"last_event_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// PageMonitor monitors a specific page pattern under a counter.
type PageMonitor struct {
	ID         int64     `json:"id"`
	CounterID  int64     `json:"counter_id"`
	Name       string    `json:"name"`
	URLPattern string    `json:"url_pattern"` // glob pattern, e.g. "/checkout*"
	Metrics    []string  `json:"metrics"`     // which metrics to track: visits, bounces, goals, revenue, errors
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
}

// Trigger defines a condition that fires an alert.
type Trigger struct {
	ID        int64     `json:"id"`
	CounterID int64     `json:"counter_id"`
	MonitorID *int64    `json:"monitor_id,omitempty"` // nil = applies to whole counter
	Name      string    `json:"name"`
	Condition string    `json:"condition"`        // event condition, e.g. "status_code == 500"
	Threshold int       `json:"threshold"`        // minimum event count in window to fire
	Window    int       `json:"window_minutes"`   // evaluation window in minutes
	Cooldown  int       `json:"cooldown_minutes"` // min minutes between repeated alerts for same trigger
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// Alert is a fired notification.
type Alert struct {
	ID         int64     `json:"id"`
	TriggerID  int64     `json:"trigger_id"`
	CounterID  int64     `json:"counter_id"`
	Title      string    `json:"title"`
	Message    string    `json:"message"`
	EventCount int       `json:"event_count"`
	CreatedAt  time.Time `json:"created_at"`
}

// ReportSnapshot stores a periodic report for comparison.
type ReportSnapshot struct {
	ID           int64          `json:"id"`
	CounterID    int64          `json:"counter_id"`
	MonitorID    *int64         `json:"monitor_id,omitempty"`
	Period       string         `json:"period"`     // "hour", "day"
	PeriodKey    string         `json:"period_key"` // "2026-08-01T10" or "2026-08-01"
	TakenAt      time.Time      `json:"taken_at"`
	Visits       int            `json:"visits"`
	UniqueVisits int            `json:"unique_visits"`
	Bounces      int            `json:"bounces"`
	AvgDuration  float64        `json:"avg_duration_sec"`
	Depth        float64        `json:"avg_depth"`
	Goals        map[string]int `json:"goals"` // goalID -> count
	Revenue      float64        `json:"revenue"`
	Orders       int            `json:"orders"`
}

// AlertAction is a delivery destination for alerts.
type AlertAction struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`              // "telegram", "vkteams" or "webhook"
	ChatID *int64 `json:"chat_id,omitempty"` // numeric chat, telegram only
	Target string `json:"target,omitempty"`  // VK Teams chatId (email- or numeric-like string)
	URL    string `json:"url,omitempty"`     // for webhook type
}

// Destination returns the human-readable delivery address of the action.
func (a AlertAction) Destination() string {
	switch a.Type {
	case "telegram":
		if a.ChatID != nil {
			return strconv.FormatInt(*a.ChatID, 10)
		}
		return a.Target
	case "vkteams":
		return a.Target
	default:
		return a.URL
	}
}

// MetrikaEvent represents a single event from Logs API.
type MetrikaEvent struct {
	CounterID string            `json:"counter_id"`
	EventTime time.Time         `json:"event_time"`
	PageURL   string            `json:"page_url"`
	Title     string            `json:"title"`
	GoalsID   []string          `json:"goals_id"`
	Status    string            `json:"status_code"`
	Revenue   float64           `json:"revenue"`
	OrderID   string            `json:"order_id"`
	ClientID  string            `json:"client_id"`
	UserID    string            `json:"user_id"`
	Params    map[string]string `json:"params"`
}

// LogRequest tracks one asynchronous Logs API export through its lifecycle.
type LogRequest struct {
	ID        int64     `json:"id"`
	CounterID int64     `json:"counter_id"`
	RequestID int64     `json:"request_id"`
	Date1     time.Time `json:"date1"`
	Date2     time.Time `json:"date2"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
