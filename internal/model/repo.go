package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---- Counters ----

func (db *DB) CreateCounter(ctx context.Context, c *Counter) error {
	res, err := db.ExecContext(ctx,
		`INSERT INTO counters (name, counter_id, oauth_token, poll_interval_minutes) VALUES (?, ?, ?, ?)`,
		c.Name, c.CounterID, c.OAuthToken, c.PollInterval,
	)
	if err != nil {
		return fmt.Errorf("create counter: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	c.ID = id
	c.CreatedAt = time.Now()
	return nil
}

func (db *DB) ListCounters(ctx context.Context) ([]Counter, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, name, counter_id, oauth_token, poll_interval_minutes, created_at FROM counters ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list counters: %w", err)
	}
	defer rows.Close()

	var out []Counter
	for rows.Next() {
		var c Counter
		if err := rows.Scan(&c.ID, &c.Name, &c.CounterID, &c.OAuthToken, &c.PollInterval, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (db *DB) GetCounter(ctx context.Context, id int64) (*Counter, error) {
	var c Counter
	err := db.QueryRowContext(ctx,
		`SELECT id, name, counter_id, oauth_token, poll_interval_minutes, created_at FROM counters WHERE id = ?`, id,
	).Scan(&c.ID, &c.Name, &c.CounterID, &c.OAuthToken, &c.PollInterval, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("counter %d not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("get counter: %w", err)
	}
	return &c, nil
}

func (db *DB) DeleteCounter(ctx context.Context, id int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM counters WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete counter: %w", err)
	}
	return nil
}

// ---- PageMonitors ----

func (db *DB) CreateMonitor(ctx context.Context, m *PageMonitor) error {
	metrics := strings.Join(m.Metrics, ",")
	res, err := db.ExecContext(ctx,
		`INSERT INTO page_monitors (counter_id, name, url_pattern, metrics, enabled) VALUES (?, ?, ?, ?, ?)`,
		m.CounterID, m.Name, m.URLPattern, metrics, m.Enabled,
	)
	if err != nil {
		return fmt.Errorf("create monitor: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	m.ID = id
	m.CreatedAt = time.Now()
	return nil
}

func (db *DB) ListMonitors(ctx context.Context, counterID int64) ([]PageMonitor, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, counter_id, name, url_pattern, metrics, enabled, created_at FROM page_monitors WHERE counter_id = ? ORDER BY id`, counterID,
	)
	if err != nil {
		return nil, fmt.Errorf("list monitors: %w", err)
	}
	defer rows.Close()

	var out []PageMonitor
	for rows.Next() {
		var m PageMonitor
		var metricsStr string
		if err := rows.Scan(&m.ID, &m.CounterID, &m.Name, &m.URLPattern, &metricsStr, &m.Enabled, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.Metrics = strings.Split(metricsStr, ",")
		out = append(out, m)
	}
	return out, rows.Err()
}

func (db *DB) UpdateMonitor(ctx context.Context, id int64, enabled bool) error {
	_, err := db.ExecContext(ctx, `UPDATE page_monitors SET enabled = ? WHERE id = ?`, enabled, id)
	if err != nil {
		return fmt.Errorf("update monitor: %w", err)
	}
	return nil
}

func (db *DB) DeleteMonitor(ctx context.Context, id int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM page_monitors WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete monitor: %w", err)
	}
	return nil
}

// ---- Triggers ----

func (db *DB) CreateTrigger(ctx context.Context, t *Trigger) error {
	res, err := db.ExecContext(ctx,
		`INSERT INTO triggers (counter_id, monitor_id, name, condition, threshold, window_minutes, cooldown_minutes, enabled) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				t.CounterID, sql.NullInt64{Valid: t.MonitorID != nil, Int64: func() int64 { if t.MonitorID != nil { return *t.MonitorID }; return 0 }()}, t.Name, t.Condition, t.Threshold, t.Window, t.Cooldown, t.Enabled,
	)
	if err != nil {
		return fmt.Errorf("create trigger: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	t.ID = id
	t.CreatedAt = time.Now()
	return nil
}

func (db *DB) ListTriggers(ctx context.Context, counterID int64) ([]Trigger, error) {
	// Verify the counter exists — a query against triggers alone would silently
	// return an empty list for a stale counter ID.
	var name string
	if err := db.QueryRowContext(ctx, `SELECT name FROM counters WHERE id = ?`, counterID).Scan(&name); err != nil {
		return nil, fmt.Errorf("counter %d not found: %w", counterID, err)
	}

	rows, err := db.QueryContext(ctx,
		`SELECT id, counter_id, monitor_id, name, condition, threshold, window_minutes, cooldown_minutes, enabled, created_at FROM triggers WHERE counter_id = ? ORDER BY id`, counterID,
	)
	if err != nil {
		return nil, fmt.Errorf("list triggers: %w", err)
	}
	defer rows.Close()

	var out []Trigger
	for rows.Next() {
		var t Trigger
		var mid sql.NullInt64
		if err := rows.Scan(&t.ID, &t.CounterID, &mid, &t.Name, &t.Condition, &t.Threshold, &t.Window, &t.Cooldown, &t.Enabled, &t.CreatedAt); err != nil {
			return nil, err
		}
		if mid.Valid {
			t.MonitorID = &mid.Int64
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (db *DB) GetTrigger(ctx context.Context, id int64) (*Trigger, error) {
	var t Trigger
	var mid sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT id, counter_id, monitor_id, name, condition, threshold, window_minutes, cooldown_minutes, enabled, created_at FROM triggers WHERE id = ?`, id,
	).Scan(&t.ID, &t.CounterID, &mid, &t.Name, &t.Condition, &t.Threshold, &t.Window, &t.Cooldown, &t.Enabled, &t.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("trigger %d not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("get trigger: %w", err)
	}
	if mid.Valid {
		t.MonitorID = &mid.Int64
	}
	return &t, nil
}

func (db *DB) UpdateTrigger(ctx context.Context, id int64, enabled bool) error {
	_, err := db.ExecContext(ctx, `UPDATE triggers SET enabled = ? WHERE id = ?`, enabled, id)
	if err != nil {
		return fmt.Errorf("update trigger: %w", err)
	}
	return nil
}

func (db *DB) DeleteTrigger(ctx context.Context, id int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM triggers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete trigger: %w", err)
	}
	return nil
}

// ---- AlertActions ----

func (db *DB) CreateAlertAction(ctx context.Context, a *AlertAction) error {
	res, err := db.ExecContext(ctx,
		`INSERT INTO alert_actions (name, type, chat_id, url) VALUES (?, ?, ?, ?)`,
		a.Name, a.Type, sql.NullInt64{Int64: *a.ChatID, Valid: a.ChatID != nil}, &a.URL,
	)
	if err != nil {
		return fmt.Errorf("create alert action: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	a.ID = id
	return nil
}

func (db *DB) ListAlertActions(ctx context.Context) ([]AlertAction, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, name, type, chat_id, url FROM alert_actions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list alert actions: %w", err)
	}
	defer rows.Close()

	var out []AlertAction
	for rows.Next() {
		var a AlertAction
		var cid sql.NullInt64
		if err := rows.Scan(&a.ID, &a.Name, &a.Type, &cid, &a.URL); err != nil {
			return nil, err
		}
		if cid.Valid {
			a.ChatID = &cid.Int64
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (db *DB) DeleteAlertAction(ctx context.Context, id int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM alert_actions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete alert action: %w", err)
	}
	return nil
}

// ---- Alerts ----

func (db *DB) CreateAlert(ctx context.Context, a *Alert) error {
	res, err := db.ExecContext(ctx,
		`INSERT INTO alerts (trigger_id, counter_id, title, message, event_count) VALUES (?, ?, ?, ?, ?)`,
		a.TriggerID, a.CounterID, a.Title, &a.Message, a.EventCount,
	)
	if err != nil {
		return fmt.Errorf("create alert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	a.ID = id
	a.CreatedAt = time.Now()
	return nil
}

func (db *DB) RecentAlerts(ctx context.Context, counterID int64, limit int) ([]Alert, error) {
	query := `SELECT id, trigger_id, counter_id, title, message, event_count, created_at FROM alerts`
	var args []any
	if counterID > 0 {
		query += ` WHERE counter_id = ?`
		args = append(args, counterID)
	}
	query += fmt.Sprintf(` ORDER BY created_at DESC LIMIT %d`, limit)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query alerts: %w", err)
	}
	defer rows.Close()

	var out []Alert
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.TriggerID, &a.CounterID, &a.Title, &a.Message, &a.EventCount, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- Trigger Cooldowns ----

func (db *DB) TriggerFiredAt(ctx context.Context, triggerID int64) (time.Time, bool, error) {
	var t time.Time
	err := db.QueryRowContext(ctx, `SELECT last_fired_at FROM trigger_cooldowns WHERE trigger_id = ?`, triggerID).Scan(&t)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("get cooldown: %w", err)
	}
	return t, true, nil
}

func (db *DB) RecordTriggerFire(ctx context.Context, triggerID int64) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO trigger_cooldowns (trigger_id, last_fired_at) VALUES (?, CURRENT_TIMESTAMP)
		 ON CONFLICT(trigger_id) DO UPDATE SET last_fired_at = CURRENT_TIMESTAMP`,
		triggerID,
	)
	if err != nil {
		return fmt.Errorf("record trigger fire: %w", err)
	}
	return nil
}

// ---- Report Snapshots ----

func (db *DB) CreateReportSnapshot(ctx context.Context, s *ReportSnapshot) error {
	goalsJSON, err := json.Marshal(s.Goals)
	if err != nil {
		return fmt.Errorf("marshal goals: %w", err)
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO report_snapshots (counter_id, monitor_id, period, period_key, taken_at, visits, unique_visits, bounces, avg_duration_sec, avg_depth, goals, revenue, orders) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.CounterID, sql.NullInt64{Int64: *s.MonitorID, Valid: s.MonitorID != nil}, s.Period, s.PeriodKey, s.TakenAt, s.Visits, s.UniqueVisits, s.Bounces, s.AvgDuration, s.Depth, string(goalsJSON), s.Revenue, s.Orders,
	)
	if err != nil {
		return fmt.Errorf("create report snapshot: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	s.ID = id
	return nil
}

func (db *DB) GetSnapshot(ctx context.Context, counterID int64, monitorID *int64, period, key string) (*ReportSnapshot, error) {
	var s ReportSnapshot
	var goalsJSON string
	var mid sql.NullInt64
	if monitorID != nil {
		err := db.QueryRowContext(ctx,
			`SELECT id, counter_id, monitor_id, period, period_key, taken_at, visits, unique_visits, bounces, avg_duration_sec, avg_depth, goals, revenue, orders FROM report_snapshots WHERE counter_id = ? AND monitor_id = ? AND period = ? AND period_key = ?`,
			counterID, *monitorID, period, key,
		).Scan(&s.ID, &s.CounterID, &mid, &s.Period, &s.PeriodKey, &s.TakenAt, &s.Visits, &s.UniqueVisits, &s.Bounces, &s.AvgDuration, &s.Depth, &goalsJSON, &s.Revenue, &s.Orders)
		if err != nil {
			return nil, err
		}
	} else {
		err := db.QueryRowContext(ctx,
			`SELECT id, counter_id, monitor_id, period, period_key, taken_at, visits, unique_visits, bounces, avg_duration_sec, avg_depth, goals, revenue, orders FROM report_snapshots WHERE counter_id = ? AND monitor_id IS NULL AND period = ? AND period_key = ?`,
			counterID, period, key,
		).Scan(&s.ID, &s.CounterID, &mid, &s.Period, &s.PeriodKey, &s.TakenAt, &s.Visits, &s.UniqueVisits, &s.Bounces, &s.AvgDuration, &s.Depth, &goalsJSON, &s.Revenue, &s.Orders)
		if err != nil {
			return nil, err
		}
	}
	s.MonitorID = &mid.Int64
	if mid.Valid {
		s.MonitorID = &mid.Int64
	} else {
		s.MonitorID = nil
	}
	if err := json.Unmarshal([]byte(goalsJSON), &s.Goals); err != nil {
		return nil, fmt.Errorf("unmarshal goals: %w", err)
	}
	return &s, nil
}

func (db *DB) ListSnapshots(ctx context.Context, counterID int64, period string, limit int) ([]ReportSnapshot, error) {
	query := fmt.Sprintf(`SELECT id, counter_id, monitor_id, period, period_key, taken_at, visits, unique_visits, bounces, avg_duration_sec, avg_depth, goals, revenue, orders FROM report_snapshots WHERE counter_id = ? AND period = ? ORDER BY period_key DESC LIMIT %d`, limit)
	rows, err := db.QueryContext(ctx, query, counterID, period)
	if err != nil {
		return nil, fmt.Errorf("query snapshots: %w", err)
	}
	defer rows.Close()

	var out []ReportSnapshot
	for rows.Next() {
		var s ReportSnapshot
		var goalsJSON string
		var mid sql.NullInt64
		if err := rows.Scan(&s.ID, &s.CounterID, &mid, &s.Period, &s.PeriodKey, &s.TakenAt, &s.Visits, &s.UniqueVisits, &s.Bounces, &s.AvgDuration, &s.Depth, &goalsJSON, &s.Revenue, &s.Orders); err != nil {
			return nil, err
		}
		if mid.Valid {
			s.MonitorID = &mid.Int64
		}
		json.Unmarshal([]byte(goalsJSON), &s.Goals)
		out = append(out, s)
	}
	return out, rows.Err()
}
