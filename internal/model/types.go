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
	// LastHourChecked is the start of the most recent hour already judged.
	// Nil on a counter that has never been polled.
	LastHourChecked *time.Time `json:"last_hour_checked,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

// Trigger fires when an hourly metric departs from what that slot in the week
// normally looks like.
//
// The comparison is per hour and per weekday because traffic has a strong
// weekly rhythm: a quiet Sunday 03:00 is normal, the same figure on Tuesday at
// noon is an incident. A flat threshold cannot tell those apart.
type Trigger struct {
	ID        int64  `json:"id"`
	CounterID int64  `json:"counter_id"`
	Name      string `json:"name"`
	// Metric is written as visits, users, pageviews, goals or goal:<id>.
	Metric string `json:"metric"`
	// Direction is drop, rise or both.
	Direction string `json:"direction"`
	// DeviationPct is how far from the baseline, in percent, counts as an anomaly.
	DeviationPct int `json:"deviation_percent"`
	// MinBaseline is a noise floor: slots quieter than this are never alerted
	// on, because one visit fewer out of two is a 50% drop and means nothing.
	MinBaseline int `json:"min_baseline"`
	// BaselineWeeks is how many earlier occurrences of the slot form the baseline.
	BaselineWeeks int       `json:"baseline_weeks"`
	Cooldown      int       `json:"cooldown_minutes"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
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
