package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// nullInt64 converts an optional ID into a SQL NULL-able value. Reading through
// the pointer is only safe once it is known to be non-nil.
func nullInt64(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

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
	rows, err := db.QueryContext(ctx, `SELECT id, name, counter_id, oauth_token, poll_interval_minutes, last_event_at, created_at FROM counters ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list counters: %w", err)
	}
	defer rows.Close()

	var out []Counter
	for rows.Next() {
		var c Counter
		var lastEvent sql.NullTime
		if err := rows.Scan(&c.ID, &c.Name, &c.CounterID, &c.OAuthToken, &c.PollInterval, &lastEvent, &c.CreatedAt); err != nil {
			return nil, err
		}
		if lastEvent.Valid {
			c.LastEventAt = &lastEvent.Time
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (db *DB) GetCounter(ctx context.Context, id int64) (*Counter, error) {
	var c Counter
	var lastEvent sql.NullTime
	err := db.QueryRowContext(ctx,
		`SELECT id, name, counter_id, oauth_token, poll_interval_minutes, last_event_at, created_at FROM counters WHERE id = ?`, id,
	).Scan(&c.ID, &c.Name, &c.CounterID, &c.OAuthToken, &c.PollInterval, &lastEvent, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("counter %d not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("get counter: %w", err)
	}
	if lastEvent.Valid {
		c.LastEventAt = &lastEvent.Time
	}
	return &c, nil
}

// DeleteCounter removes a counter and everything scoped to it. The schema
// declares foreign keys but SQLite does not enforce them by default, so the
// dependent rows are removed explicitly rather than left orphaned.
func (db *DB) DeleteCounter(ctx context.Context, id int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete counter: %w", err)
	}
	defer tx.Rollback()

	stmts := []string{
		`DELETE FROM log_requests WHERE counter_id = ?`,
		`DELETE FROM trigger_cooldowns WHERE trigger_id IN (SELECT id FROM triggers WHERE counter_id = ?)`,
		`DELETE FROM triggers WHERE counter_id = ?`,
		`DELETE FROM page_monitors WHERE counter_id = ?`,
		`DELETE FROM report_snapshots WHERE counter_id = ?`,
		`DELETE FROM alerts WHERE counter_id = ?`,
		`DELETE FROM counters WHERE id = ?`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return fmt.Errorf("delete counter: %w", err)
		}
	}
	return tx.Commit()
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
		t.CounterID, nullInt64(t.MonitorID), t.Name, t.Condition, t.Threshold, t.Window, t.Cooldown, t.Enabled,
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
		`INSERT INTO alert_actions (name, type, chat_id, target, url) VALUES (?, ?, ?, ?, ?)`,
		a.Name, a.Type, nullInt64(a.ChatID), a.Target, a.URL,
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
	rows, err := db.QueryContext(ctx, `SELECT id, name, type, chat_id, target, url FROM alert_actions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list alert actions: %w", err)
	}
	defer rows.Close()

	var out []AlertAction
	for rows.Next() {
		var a AlertAction
		var cid sql.NullInt64
		var url sql.NullString
		if err := rows.Scan(&a.ID, &a.Name, &a.Type, &cid, &a.Target, &url); err != nil {
			return nil, err
		}
		a.URL = url.String
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
		a.TriggerID, a.CounterID, a.Title, a.Message, a.EventCount,
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
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query alerts: %w", err)
	}
	defer rows.Close()

	var out []Alert
	for rows.Next() {
		var a Alert
		var message sql.NullString
		if err := rows.Scan(&a.ID, &a.TriggerID, &a.CounterID, &a.Title, &message, &a.EventCount, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.Message = message.String
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
		s.CounterID, nullInt64(s.MonitorID), s.Period, s.PeriodKey, s.TakenAt, s.Visits, s.UniqueVisits, s.Bounces, s.AvgDuration, s.Depth, string(goalsJSON), s.Revenue, s.Orders,
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
	if mid.Valid {
		s.MonitorID = &mid.Int64
	}
	if err := json.Unmarshal([]byte(goalsJSON), &s.Goals); err != nil {
		return nil, fmt.Errorf("unmarshal goals: %w", err)
	}
	return &s, nil
}

func (db *DB) ListSnapshots(ctx context.Context, counterID int64, period string, limit int) ([]ReportSnapshot, error) {
	const query = `SELECT id, counter_id, monitor_id, period, period_key, taken_at, visits, unique_visits, bounces, avg_duration_sec, avg_depth, goals, revenue, orders FROM report_snapshots WHERE counter_id = ? AND period = ? ORDER BY period_key DESC LIMIT ?`
	rows, err := db.QueryContext(ctx, query, counterID, period, limit)
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

// ---- Log requests ----
//
// A Logs API export is ordered in one poll and downloaded in a later one, so
// its identity has to survive both the poll and the process.

// CreateLogRequest records a newly ordered export.
func (db *DB) CreateLogRequest(ctx context.Context, r *LogRequest) error {
	res, err := db.ExecContext(ctx,
		`INSERT INTO log_requests (counter_id, request_id, date1, date2, status) VALUES (?, ?, ?, ?, ?)`,
		r.CounterID, r.RequestID, r.Date1, r.Date2, r.Status,
	)
	if err != nil {
		return fmt.Errorf("create log request: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	r.ID = id
	return nil
}

// PendingLogRequest returns the export a counter is currently waiting on, or
// nil when there is none. Only one is in flight at a time: exports are ordered
// for consecutive windows, and running several would reorder the event stream.
func (db *DB) PendingLogRequest(ctx context.Context, counterID int64) (*LogRequest, error) {
	var r LogRequest
	err := db.QueryRowContext(ctx,
		`SELECT id, counter_id, request_id, date1, date2, status, created_at, updated_at
		 FROM log_requests WHERE counter_id = ? ORDER BY id DESC LIMIT 1`, counterID,
	).Scan(&r.ID, &r.CounterID, &r.RequestID, &r.Date1, &r.Date2, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get pending log request: %w", err)
	}
	return &r, nil
}

// UpdateLogRequestStatus records the state Metrika last reported.
func (db *DB) UpdateLogRequestStatus(ctx context.Context, id int64, status string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE log_requests SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("update log request status: %w", err)
	}
	return nil
}

// DeleteLogRequest drops a finished export from tracking.
func (db *DB) DeleteLogRequest(ctx context.Context, id int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM log_requests WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete log request: %w", err)
	}
	return nil
}

// StaleLogRequests lists exports last touched before cutoff, across all
// counters. A process killed mid-flight leaves its request behind, and Metrika
// caps how many a counter may hold — these are the ones to cancel.
func (db *DB) StaleLogRequests(ctx context.Context, cutoff time.Time) ([]LogRequest, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, counter_id, request_id, date1, date2, status, created_at, updated_at
		 FROM log_requests WHERE updated_at < ? ORDER BY id`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("list stale log requests: %w", err)
	}
	defer rows.Close()

	var out []LogRequest
	for rows.Next() {
		var r LogRequest
		if err := rows.Scan(&r.ID, &r.CounterID, &r.RequestID, &r.Date1, &r.Date2, &r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- Watermark ----

// SetCounterWatermark records the newest event time already evaluated.
func (db *DB) SetCounterWatermark(ctx context.Context, counterID int64, t time.Time) error {
	_, err := db.ExecContext(ctx, `UPDATE counters SET last_event_at = ? WHERE id = ?`, t, counterID)
	if err != nil {
		return fmt.Errorf("set watermark: %w", err)
	}
	return nil
}
