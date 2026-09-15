package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
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
	rows, err := db.QueryContext(ctx, `SELECT id, name, counter_id, oauth_token, poll_interval_minutes, last_hour_checked, created_at FROM counters ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list counters: %w", err)
	}
	defer rows.Close()

	var out []Counter
	for rows.Next() {
		var c Counter
		var lastHour sql.NullTime
		if err := rows.Scan(&c.ID, &c.Name, &c.CounterID, &c.OAuthToken, &c.PollInterval, &lastHour, &c.CreatedAt); err != nil {
			return nil, err
		}
		if lastHour.Valid {
			c.LastHourChecked = &lastHour.Time
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (db *DB) GetCounter(ctx context.Context, id int64) (*Counter, error) {
	var c Counter
	var lastHour sql.NullTime
	err := db.QueryRowContext(ctx,
		`SELECT id, name, counter_id, oauth_token, poll_interval_minutes, last_hour_checked, created_at FROM counters WHERE id = ?`, id,
	).Scan(&c.ID, &c.Name, &c.CounterID, &c.OAuthToken, &c.PollInterval, &lastHour, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("counter %d not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("get counter: %w", err)
	}
	if lastHour.Valid {
		c.LastHourChecked = &lastHour.Time
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
		`DELETE FROM reports WHERE counter_id = ?`,
		`DELETE FROM trigger_cooldowns WHERE trigger_id IN (SELECT id FROM triggers WHERE counter_id = ?)`,
		`DELETE FROM triggers WHERE counter_id = ?`,
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

// ---- Triggers ----

const triggerColumns = `id, counter_id, name, metric, direction, deviation_percent, min_baseline, baseline_weeks, url_filter, url_match, cooldown_minutes, enabled, created_at`

func scanTrigger(row interface{ Scan(...any) error }) (Trigger, error) {
	var t Trigger
	err := row.Scan(&t.ID, &t.CounterID, &t.Name, &t.Metric, &t.Direction,
		&t.DeviationPct, &t.MinBaseline, &t.BaselineWeeks, &t.URLFilter, &t.URLMatch,
		&t.Cooldown, &t.Enabled, &t.CreatedAt)
	return t, err
}

func (db *DB) CreateTrigger(ctx context.Context, t *Trigger) error {
	res, err := db.ExecContext(ctx,
		`INSERT INTO triggers (counter_id, name, metric, direction, deviation_percent, min_baseline, baseline_weeks, url_filter, url_match, cooldown_minutes, enabled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.CounterID, t.Name, t.Metric, t.Direction, t.DeviationPct, t.MinBaseline, t.BaselineWeeks,
		t.URLFilter, t.URLMatch, t.Cooldown, t.Enabled,
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
		`SELECT `+triggerColumns+` FROM triggers WHERE counter_id = ? ORDER BY id`, counterID)
	if err != nil {
		return nil, fmt.Errorf("list triggers: %w", err)
	}
	defer rows.Close()

	var out []Trigger
	for rows.Next() {
		t, err := scanTrigger(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (db *DB) GetTrigger(ctx context.Context, id int64) (*Trigger, error) {
	row := db.QueryRowContext(ctx, `SELECT `+triggerColumns+` FROM triggers WHERE id = ?`, id)
	t, err := scanTrigger(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("trigger %d not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("get trigger: %w", err)
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
		`INSERT INTO report_snapshots (counter_id, period, period_key, taken_at, visits, unique_visits, bounces, avg_duration_sec, avg_depth, goals, revenue, orders) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.CounterID, s.Period, s.PeriodKey, s.TakenAt, s.Visits, s.UniqueVisits, s.Bounces, s.AvgDuration, s.Depth, string(goalsJSON), s.Revenue, s.Orders,
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

const snapshotColumns = `id, counter_id, period, period_key, taken_at, visits, unique_visits, bounces, avg_duration_sec, avg_depth, goals, revenue, orders`

func (db *DB) GetSnapshot(ctx context.Context, counterID int64, period, key string) (*ReportSnapshot, error) {
	var s ReportSnapshot
	var goalsJSON string
	err := db.QueryRowContext(ctx,
		`SELECT `+snapshotColumns+` FROM report_snapshots WHERE counter_id = ? AND period = ? AND period_key = ?`,
		counterID, period, key,
	).Scan(&s.ID, &s.CounterID, &s.Period, &s.PeriodKey, &s.TakenAt, &s.Visits, &s.UniqueVisits, &s.Bounces, &s.AvgDuration, &s.Depth, &goalsJSON, &s.Revenue, &s.Orders)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(goalsJSON), &s.Goals); err != nil {
		return nil, fmt.Errorf("unmarshal goals: %w", err)
	}
	return &s, nil
}

func (db *DB) ListSnapshots(ctx context.Context, counterID int64, period string, limit int) ([]ReportSnapshot, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+snapshotColumns+` FROM report_snapshots WHERE counter_id = ? AND period = ? ORDER BY period_key DESC LIMIT ?`,
		counterID, period, limit)
	if err != nil {
		return nil, fmt.Errorf("query snapshots: %w", err)
	}
	defer rows.Close()

	var out []ReportSnapshot
	for rows.Next() {
		var s ReportSnapshot
		var goalsJSON string
		if err := rows.Scan(&s.ID, &s.CounterID, &s.Period, &s.PeriodKey, &s.TakenAt, &s.Visits, &s.UniqueVisits, &s.Bounces, &s.AvgDuration, &s.Depth, &goalsJSON, &s.Revenue, &s.Orders); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(goalsJSON), &s.Goals)
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---- Hour cursor ----

// SetLastHourChecked records the most recent hour already judged, so a restart
// neither re-alerts on it nor skips the hours in between.
func (db *DB) SetLastHourChecked(ctx context.Context, counterID int64, hour time.Time) error {
	_, err := db.ExecContext(ctx, `UPDATE counters SET last_hour_checked = ? WHERE id = ?`, hour, counterID)
	if err != nil {
		return fmt.Errorf("set last checked hour: %w", err)
	}
	return nil
}

// ---- Settings ----
//
// Runtime settings live in the database so the bot can change them without a
// config edit and a restart, and so they survive one.

// Setting keys.
const (
	// SettingReportSchedule is when periodic reports are sent.
	SettingReportSchedule = "report_schedule"
	// SettingReportLastRun is when the last periodic report went out, so a
	// restart neither repeats one nor skips one.
	SettingReportLastRun = "report_last_run"
)

// Setting returns a stored value, or ok=false when it was never set.
func (db *DB) Setting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get setting %s: %w", key, err)
	}
	return value, true, nil
}

// SetSetting stores a value.
func (db *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
		key, value)
	if err != nil {
		return fmt.Errorf("set setting %s: %w", key, err)
	}
	return nil
}

// SettingTime reads a stored timestamp, or the zero time when unset.
func (db *DB) SettingTime(ctx context.Context, key string) (time.Time, error) {
	raw, ok, err := db.Setting(ctx, key)
	if err != nil || !ok {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		// A corrupted timestamp must not wedge the schedule forever; treating
		// it as "never ran" makes the next tick recover.
		return time.Time{}, nil
	}
	return t, nil
}

// SetSettingTime stores a timestamp.
func (db *DB) SetSettingTime(ctx context.Context, key string, t time.Time) error {
	return db.SetSetting(ctx, key, t.Format(time.RFC3339))
}

// ---- Reports ----

const reportColumns = `id, counter_id, name, url_filter, url_match, goal_ids, group_by, enabled, created_at`

func scanReport(row interface{ Scan(...any) error }) (Report, error) {
	var r Report
	var goalIDs string
	err := row.Scan(&r.ID, &r.CounterID, &r.Name, &r.URLFilter, &r.URLMatch, &goalIDs, &r.GroupBy, &r.Enabled, &r.CreatedAt)
	if err != nil {
		return r, err
	}
	r.GoalIDs = parseGoalIDs(goalIDs)
	return r, nil
}

// parseGoalIDs reads the stored comma-separated list, skipping anything
// unreadable rather than failing the whole row.
func parseGoalIDs(s string) []int64 {
	var out []int64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if id, err := strconv.ParseInt(part, 10, 64); err == nil && id > 0 {
			out = append(out, id)
		}
	}
	return out
}

func formatGoalIDs(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}

func (db *DB) CreateReport(ctx context.Context, r *Report) error {
	res, err := db.ExecContext(ctx,
		`INSERT INTO reports (counter_id, name, url_filter, url_match, goal_ids, group_by, enabled)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.CounterID, r.Name, r.URLFilter, r.URLMatch, formatGoalIDs(r.GoalIDs), r.GroupBy, r.Enabled,
	)
	if err != nil {
		return fmt.Errorf("create report: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	r.ID = id
	r.CreatedAt = time.Now()
	return nil
}

// ListReports returns the reports for one counter, or for every counter when
// counterID is zero.
func (db *DB) ListReports(ctx context.Context, counterID int64) ([]Report, error) {
	query := `SELECT ` + reportColumns + ` FROM reports`
	var args []any
	if counterID > 0 {
		query += ` WHERE counter_id = ?`
		args = append(args, counterID)
	}
	query += ` ORDER BY id`

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list reports: %w", err)
	}
	defer rows.Close()

	var out []Report
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (db *DB) DeleteReport(ctx context.Context, id int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM reports WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete report: %w", err)
	}
	return nil
}
