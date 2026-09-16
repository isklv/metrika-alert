package model

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FlexString decodes a string that may be serialized as a JSON string or number.
type FlexString string

func (f *FlexString) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) >= 2 && data[0] == '"' && data[len(data)-1] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*f = FlexString(s)
		return nil
	}
	*f = FlexString(string(data))
	return nil
}

func (f FlexString) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(f))
}

func (f *FlexString) UnmarshalYAML(value *yaml.Node) error {
	*f = FlexString(value.Value)
	return nil
}

func (f FlexString) String() string {
	return string(f)
}

// ExportData is the full portable representation of the bot and monitoring settings.
type ExportData struct {
	Version      int                 `json:"version" yaml:"version"`
	ExportedAt   time.Time           `json:"exported_at" yaml:"exported_at"`
	Schedule     string              `json:"schedule,omitempty" yaml:"schedule,omitempty"`
	Counters     []ExportCounter     `json:"counters,omitempty" yaml:"counters,omitempty"`
	Triggers     []ExportFlatTrigger `json:"triggers,omitempty" yaml:"triggers,omitempty"`
	Reports      []ExportFlatReport  `json:"reports,omitempty" yaml:"reports,omitempty"`
	AlertActions []ExportAction      `json:"alert_actions,omitempty" yaml:"alert_actions,omitempty"`
}

func (d *ExportData) hasAnySettings() bool {
	return len(d.Counters) > 0 || len(d.Triggers) > 0 || len(d.Reports) > 0 || len(d.AlertActions) > 0 || strings.TrimSpace(d.Schedule) != ""
}

// ExportCounter represents a counter and its associated rules in an export.
type ExportCounter struct {
	Name         string          `json:"name" yaml:"name"`
	CounterID    FlexString      `json:"counter_id" yaml:"counter_id"`
	OAuthToken   string          `json:"oauth_token,omitempty" yaml:"oauth_token,omitempty"`
	PollInterval int             `json:"poll_interval_minutes,omitempty" yaml:"poll_interval_minutes,omitempty"`
	Triggers     []ExportTrigger `json:"triggers,omitempty" yaml:"triggers,omitempty"`
	Reports      []ExportReport  `json:"reports,omitempty" yaml:"reports,omitempty"`
}

// ExportTrigger defines an anomaly alert trigger in an export.
type ExportTrigger struct {
	Name          string `json:"name" yaml:"name"`
	Metric        string `json:"metric" yaml:"metric"`
	Direction     string `json:"direction" yaml:"direction"`
	DeviationPct  int    `json:"deviation_percent" yaml:"deviation_percent"`
	MinBaseline   int    `json:"min_baseline,omitempty" yaml:"min_baseline,omitempty"`
	BaselineWeeks int    `json:"baseline_weeks,omitempty" yaml:"baseline_weeks,omitempty"`
	URLFilter     string `json:"url_filter,omitempty" yaml:"url_filter,omitempty"`
	URLMatch      string `json:"url_match,omitempty" yaml:"url_match,omitempty"`
	Cooldown      int    `json:"cooldown_minutes,omitempty" yaml:"cooldown_minutes,omitempty"`
	Enabled       *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// ExportFlatTrigger allows triggers to be specified in a flat array by counter_id.
type ExportFlatTrigger struct {
	CounterID FlexString `json:"counter_id" yaml:"counter_id"`
	ExportTrigger
}

// ExportReport defines a periodic report in an export.
type ExportReport struct {
	Name      string  `json:"name" yaml:"name"`
	URLFilter string  `json:"url_filter,omitempty" yaml:"url_filter,omitempty"`
	URLMatch  string  `json:"url_match,omitempty" yaml:"url_match,omitempty"`
	GoalIDs   []int64 `json:"goal_ids,omitempty" yaml:"goal_ids,omitempty"`
	GroupBy   string  `json:"group_by,omitempty" yaml:"group_by,omitempty"`
	Period    string  `json:"period,omitempty" yaml:"period,omitempty"`
	Enabled   *bool   `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// ExportFlatReport allows reports to be specified in a flat array by counter_id.
type ExportFlatReport struct {
	CounterID FlexString `json:"counter_id" yaml:"counter_id"`
	ExportReport
}

// ExportAction defines an alert destination in an export.
type ExportAction struct {
	Name        string `json:"name,omitempty" yaml:"name,omitempty"`
	Type        string `json:"type" yaml:"type"`
	ChatID      *int64 `json:"chat_id,omitempty" yaml:"chat_id,omitempty"`
	Target      string `json:"target,omitempty" yaml:"target,omitempty"`
	URL         string `json:"url,omitempty" yaml:"url,omitempty"`
	Destination string `json:"destination,omitempty" yaml:"destination,omitempty"`
}

// ImportResult summarizes the outcome of applying settings.
type ImportResult struct {
	CountersCreated int
	CountersUpdated int
	TriggersCreated int
	TriggersUpdated int
	ReportsCreated  int
	ReportsUpdated  int
	ActionsCreated  int
	ActionsUpdated  int
	ScheduleUpdated bool
	Schedule        string
	Warnings        []string
}

// ExportValidator validates application-level rules like metrics and schedules.
type ExportValidator interface {
	ValidateMetric(metric string) error
	ValidateSchedule(schedule string) error
	ValidatePeriod(period string) error
}

// UnmarshalExportData parses raw configuration in JSON or YAML format.
func UnmarshalExportData(raw []byte) (*ExportData, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("пустые данные")
	}

	// Strip markdown code fences if present (e.g. ```json ... ```)
	s := string(raw)
	if strings.HasPrefix(s, "```") {
		lines := strings.Split(s, "\n")
		if len(lines) >= 2 && strings.HasPrefix(lines[0], "```") {
			last := strings.TrimSpace(lines[len(lines)-1])
			if strings.HasSuffix(last, "```") {
				lines = lines[1 : len(lines)-1]
				raw = bytes.TrimSpace([]byte(strings.Join(lines, "\n")))
			}
		}
	}

	var data ExportData
	if err := json.Unmarshal(raw, &data); err == nil && data.hasAnySettings() {
		return &data, nil
	}

	var yamlData ExportData
	if err := yaml.Unmarshal(raw, &yamlData); err == nil && yamlData.hasAnySettings() {
		return &yamlData, nil
	}

	var testJSON any
	if err := json.Unmarshal(raw, &testJSON); err != nil {
		return nil, fmt.Errorf("неверный формат JSON: %w", err)
	}
	return nil, fmt.Errorf("в данных не найдено настроек (counters, triggers, reports, alert_actions, schedule)")
}

// Export extracts configuration from the database. If counterID > 0, it exports only that counter.
func (db *DB) Export(ctx context.Context, counterID int64, includeTokens bool) (*ExportData, error) {
	data := &ExportData{
		Version:    1,
		ExportedAt: time.Now().UTC(),
	}

	if counterID == 0 {
		if rawSchedule, ok, err := db.Setting(ctx, SettingReportSchedule); err == nil && ok {
			data.Schedule = rawSchedule
		}

		actions, err := db.ListAlertActions(ctx)
		if err != nil {
			return nil, fmt.Errorf("export actions: %w", err)
		}
		for _, a := range actions {
			data.AlertActions = append(data.AlertActions, ExportAction{
				Name:   a.Name,
				Type:   a.Type,
				ChatID: a.ChatID,
				Target: a.Target,
				URL:    a.URL,
			})
		}
	}

	var counters []Counter
	if counterID > 0 {
		c, err := db.GetCounter(ctx, counterID)
		if err != nil {
			return nil, fmt.Errorf("export counter %d: %w", counterID, err)
		}
		counters = append(counters, *c)
	} else {
		var err error
		counters, err = db.ListCounters(ctx)
		if err != nil {
			return nil, fmt.Errorf("export list counters: %w", err)
		}
	}

	for _, c := range counters {
		token := ""
		if includeTokens {
			token = c.OAuthToken
		}
		ec := ExportCounter{
			Name:         c.Name,
			CounterID:    FlexString(c.CounterID),
			OAuthToken:   token,
			PollInterval: c.PollInterval,
		}

		triggers, err := db.ListTriggers(ctx, c.ID)
		if err != nil {
			return nil, fmt.Errorf("export triggers for counter %d: %w", c.ID, err)
		}
		for _, t := range triggers {
			enabled := t.Enabled
			ec.Triggers = append(ec.Triggers, ExportTrigger{
				Name:          t.Name,
				Metric:        t.Metric,
				Direction:     t.Direction,
				DeviationPct:  t.DeviationPct,
				MinBaseline:   t.MinBaseline,
				BaselineWeeks: t.BaselineWeeks,
				URLFilter:     t.URLFilter,
				URLMatch:      t.URLMatch,
				Cooldown:      t.Cooldown,
				Enabled:       &enabled,
			})
		}

		reports, err := db.ListReports(ctx, c.ID)
		if err != nil {
			return nil, fmt.Errorf("export reports for counter %d: %w", c.ID, err)
		}
		for _, r := range reports {
			enabled := r.Enabled
			ec.Reports = append(ec.Reports, ExportReport{
				Name:      r.Name,
				URLFilter: r.URLFilter,
				URLMatch:  r.URLMatch,
				GoalIDs:   r.GoalIDs,
				GroupBy:   r.GroupBy,
				Period:    r.Period,
				Enabled:   &enabled,
			})
		}

		data.Counters = append(data.Counters, ec)
	}

	return data, nil
}

// Import applies settings from ExportData to the database.
func (db *DB) Import(ctx context.Context, data *ExportData, replace bool, validator ExportValidator) (*ImportResult, error) {
	if data == nil || !data.hasAnySettings() {
		return nil, fmt.Errorf("в данных нет настроек для импорта")
	}

	// 1. Validation before touching the database
	for i := range data.Counters {
		c := &data.Counters[i]
		cid := strings.TrimSpace(c.CounterID.String())
		if cid == "" {
			return nil, fmt.Errorf("счётчик %q: не указан counter_id", c.Name)
		}
		if _, err := strconv.ParseInt(cid, 10, 64); err != nil {
			return nil, fmt.Errorf("счётчик %q: counter_id %q должен быть числом", c.Name, cid)
		}
		if c.Name == "" {
			c.Name = cid
		}
		if c.PollInterval <= 0 {
			c.PollInterval = 60
		}
	}

	validateTrigger := func(t *ExportTrigger) error {
		if strings.TrimSpace(t.Name) == "" {
			return fmt.Errorf("правило алерта: имя не должно быть пустым")
		}
		if strings.TrimSpace(t.Metric) == "" {
			return fmt.Errorf("правило %q: не указана метрика", t.Name)
		}
		if validator != nil {
			if err := validator.ValidateMetric(t.Metric); err != nil {
				return fmt.Errorf("правило %q: %w", t.Name, err)
			}
		}
		if t.Direction == "" {
			t.Direction = "drop"
		}
		t.Direction = strings.ToLower(t.Direction)
		if t.Direction != "drop" && t.Direction != "rise" && t.Direction != "both" {
			return fmt.Errorf("правило %q: неверное направление %q (доступно: drop, rise, both)", t.Name, t.Direction)
		}
		if t.DeviationPct <= 0 || t.DeviationPct > 100 {
			return fmt.Errorf("правило %q: порог %d%% должен быть от 1 до 100", t.Name, t.DeviationPct)
		}
		if t.MinBaseline < 0 {
			return fmt.Errorf("правило %q: мин_база %d не может быть отрицательной", t.Name, t.MinBaseline)
		}
		if t.BaselineWeeks < 0 || t.BaselineWeeks > 12 {
			return fmt.Errorf("правило %q: недель %d должно быть от 1 до 12", t.Name, t.BaselineWeeks)
		}
		if t.BaselineWeeks == 0 {
			t.BaselineWeeks = 4
		}
		if t.Cooldown <= 0 {
			t.Cooldown = 180
		}
		if t.URLFilter != "" {
			if t.URLMatch == "" {
				t.URLMatch = "contains"
			}
			if t.URLMatch != "contains" && t.URLMatch != "regexp" {
				return fmt.Errorf("правило %q: неверный url_match %q", t.Name, t.URLMatch)
			}
			if t.URLMatch == "regexp" {
				if _, err := regexp.Compile(t.URLFilter); err != nil {
					return fmt.Errorf("правило %q: неверное регулярное выражение %q: %w", t.Name, t.URLFilter, err)
				}
			}
		}
		return nil
	}

	validateReport := func(r *ExportReport) error {
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("отчёт: имя не должно быть пустым")
		}
		if r.URLFilter != "" {
			if r.URLMatch == "" {
				r.URLMatch = "contains"
			}
			if r.URLMatch != "contains" && r.URLMatch != "regexp" {
				return fmt.Errorf("отчёт %q: неверный url_match %q", r.Name, r.URLMatch)
			}
			if r.URLMatch == "regexp" {
				if _, err := regexp.Compile(r.URLFilter); err != nil {
					return fmt.Errorf("отчёт %q: неверное регулярное выражение %q: %w", r.Name, r.URLFilter, err)
				}
			}
		}
		if r.GroupBy != "" && r.GroupBy != "url" {
			return fmt.Errorf("отчёт %q: неверная группировка %q (доступно: group=url)", r.Name, r.GroupBy)
		}
		if validator != nil && r.Period != "" {
			if err := validator.ValidatePeriod(r.Period); err != nil {
				return fmt.Errorf("отчёт %q: %w", r.Name, err)
			}
		}
		return nil
	}

	for i := range data.Counters {
		for j := range data.Counters[i].Triggers {
			if err := validateTrigger(&data.Counters[i].Triggers[j]); err != nil {
				return nil, err
			}
		}
		for j := range data.Counters[i].Reports {
			if err := validateReport(&data.Counters[i].Reports[j]); err != nil {
				return nil, err
			}
		}
	}
	for i := range data.Triggers {
		if err := validateTrigger(&data.Triggers[i].ExportTrigger); err != nil {
			return nil, err
		}
	}
	for i := range data.Reports {
		if err := validateReport(&data.Reports[i].ExportReport); err != nil {
			return nil, err
		}
	}

	for i := range data.AlertActions {
		a := &data.AlertActions[i]
		a.Type = strings.ToLower(strings.TrimSpace(a.Type))
		target := strings.TrimSpace(a.Target)
		if target == "" && a.Destination != "" {
			target = strings.TrimSpace(a.Destination)
		}
		urlVal := strings.TrimSpace(a.URL)
		if urlVal == "" && a.Destination != "" && (strings.HasPrefix(a.Destination, "http://") || strings.HasPrefix(a.Destination, "https://")) {
			urlVal = strings.TrimSpace(a.Destination)
		}

		switch a.Type {
		case "telegram":
			if a.ChatID == nil {
				if target == "" {
					return nil, fmt.Errorf("alert action: для telegram требуется chat_id")
				}
				cid, err := strconv.ParseInt(target, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("alert action: неверный chat_id %q: число должно быть числом Telegram", target)
				}
				a.ChatID = &cid
			}
			if target == "" {
				target = strconv.FormatInt(*a.ChatID, 10)
			}
			a.Target = target
			if a.Name == "" {
				a.Name = "telegram-" + target
			}
		case "vkteams":
			if target == "" {
				return nil, fmt.Errorf("alert action: для vkteams требуется target (chatId)")
			}
			a.Target = target
			if a.Name == "" {
				a.Name = "vkteams-" + target
			}
		case "webhook":
			if urlVal == "" {
				return nil, fmt.Errorf("alert action: для webhook требуется url")
			}
			if !strings.HasPrefix(urlVal, "http://") && !strings.HasPrefix(urlVal, "https://") {
				return nil, fmt.Errorf("alert action: URL вебхука должен начинаться с http:// или https://")
			}
			a.URL = urlVal
			if a.Name == "" {
				a.Name = "webhook-" + urlVal
			}
		default:
			return nil, fmt.Errorf("alert action: неизвестный тип %q (доступны: telegram, vkteams, webhook)", a.Type)
		}
	}

	if data.Schedule != "" && validator != nil {
		if err := validator.ValidateSchedule(data.Schedule); err != nil {
			return nil, fmt.Errorf("расписание: %w", err)
		}
	}

	// 2. Perform DB updates in transaction
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin import tx: %w", err)
	}
	defer tx.Rollback()

	result := &ImportResult{}

	if replace {
		stmts := []string{
			`DELETE FROM reports`,
			`DELETE FROM trigger_cooldowns`,
			`DELETE FROM triggers`,
			`DELETE FROM report_snapshots`,
			`DELETE FROM alerts`,
			`DELETE FROM counters`,
		}
		if len(data.AlertActions) > 0 {
			stmts = append(stmts, `DELETE FROM alert_actions`)
		}
		for _, q := range stmts {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return nil, fmt.Errorf("clear existing data: %w", err)
			}
		}
	}

	counterIDToDBID := make(map[string]int64)

	for _, c := range data.Counters {
		cid := strings.TrimSpace(c.CounterID.String())
		if !replace {
			var existingID int64
			var existingToken string
			err := tx.QueryRowContext(ctx, `SELECT id, oauth_token FROM counters WHERE counter_id = ?`, cid).Scan(&existingID, &existingToken)
			if err == nil {
				token := c.OAuthToken
				if token == "" {
					token = existingToken
				}
				_, err = tx.ExecContext(ctx,
					`UPDATE counters SET name = ?, oauth_token = ?, poll_interval_minutes = ? WHERE id = ?`,
					c.Name, token, c.PollInterval, existingID)
				if err != nil {
					return nil, fmt.Errorf("update counter %s: %w", cid, err)
				}
				counterIDToDBID[cid] = existingID
				result.CountersUpdated++
				continue
			} else if err != sql.ErrNoRows {
				return nil, fmt.Errorf("query counter %s: %w", cid, err)
			}
		}

		res, err := tx.ExecContext(ctx,
			`INSERT INTO counters (name, counter_id, oauth_token, poll_interval_minutes) VALUES (?, ?, ?, ?)`,
			c.Name, cid, c.OAuthToken, c.PollInterval)
		if err != nil {
			return nil, fmt.Errorf("insert counter %s: %w", cid, err)
		}
		newID, _ := res.LastInsertId()
		counterIDToDBID[cid] = newID
		result.CountersCreated++
		if c.OAuthToken == "" {
			result.Warnings = append(result.Warnings, fmt.Sprintf("Счётчик %s (%s) добавлен без OAuth-токена", c.Name, cid))
		}
	}

	// For flat triggers / reports, look up counters that might already exist in DB
	ensureCounterDBID := func(cid string) (int64, error) {
		if id, ok := counterIDToDBID[cid]; ok {
			return id, nil
		}
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM counters WHERE counter_id = ?`, cid).Scan(&id)
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("счётчик %q не найден в базе", cid)
		}
		if err != nil {
			return 0, err
		}
		counterIDToDBID[cid] = id
		return id, nil
	}

	// Apply triggers
	applyTrigger := func(dbCounterID int64, t ExportTrigger) error {
		enabled := true
		if t.Enabled != nil {
			enabled = *t.Enabled
		}
		if !replace {
			var existingID int64
			err := tx.QueryRowContext(ctx, `SELECT id FROM triggers WHERE counter_id = ? AND name = ?`, dbCounterID, t.Name).Scan(&existingID)
			if err == nil {
				_, err = tx.ExecContext(ctx,
					`UPDATE triggers SET metric = ?, direction = ?, deviation_percent = ?, min_baseline = ?, baseline_weeks = ?, url_filter = ?, url_match = ?, cooldown_minutes = ?, enabled = ? WHERE id = ?`,
					t.Metric, t.Direction, t.DeviationPct, t.MinBaseline, t.BaselineWeeks, t.URLFilter, t.URLMatch, t.Cooldown, enabled, existingID)
				if err != nil {
					return fmt.Errorf("update trigger %s: %w", t.Name, err)
				}
				result.TriggersUpdated++
				return nil
			} else if err != sql.ErrNoRows {
				return err
			}
		}

		_, err := tx.ExecContext(ctx,
			`INSERT INTO triggers (counter_id, name, metric, direction, deviation_percent, min_baseline, baseline_weeks, url_filter, url_match, cooldown_minutes, enabled)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			dbCounterID, t.Name, t.Metric, t.Direction, t.DeviationPct, t.MinBaseline, t.BaselineWeeks, t.URLFilter, t.URLMatch, t.Cooldown, enabled)
		if err != nil {
			return fmt.Errorf("insert trigger %s: %w", t.Name, err)
		}
		result.TriggersCreated++
		return nil
	}

	for _, c := range data.Counters {
		cid := strings.TrimSpace(c.CounterID.String())
		dbID := counterIDToDBID[cid]
		for _, t := range c.Triggers {
			if err := applyTrigger(dbID, t); err != nil {
				return nil, err
			}
		}
	}
	for _, ft := range data.Triggers {
		cid := strings.TrimSpace(ft.CounterID.String())
		dbID, err := ensureCounterDBID(cid)
		if err != nil {
			return nil, fmt.Errorf("правило %q: %w", ft.Name, err)
		}
		if err := applyTrigger(dbID, ft.ExportTrigger); err != nil {
			return nil, err
		}
	}

	// Apply reports
	applyReport := func(dbCounterID int64, r ExportReport) error {
		enabled := true
		if r.Enabled != nil {
			enabled = *r.Enabled
		}
		goalIDsStr := formatGoalIDs(r.GoalIDs)
		if !replace {
			var existingID int64
			err := tx.QueryRowContext(ctx, `SELECT id FROM reports WHERE counter_id = ? AND name = ?`, dbCounterID, r.Name).Scan(&existingID)
			if err == nil {
				_, err = tx.ExecContext(ctx,
					`UPDATE reports SET url_filter = ?, url_match = ?, goal_ids = ?, group_by = ?, period = ?, enabled = ? WHERE id = ?`,
					r.URLFilter, r.URLMatch, goalIDsStr, r.GroupBy, r.Period, enabled, existingID)
				if err != nil {
					return fmt.Errorf("update report %s: %w", r.Name, err)
				}
				result.ReportsUpdated++
				return nil
			} else if err != sql.ErrNoRows {
				return err
			}
		}

		_, err := tx.ExecContext(ctx,
			`INSERT INTO reports (counter_id, name, url_filter, url_match, goal_ids, group_by, period, enabled)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			dbCounterID, r.Name, r.URLFilter, r.URLMatch, goalIDsStr, r.GroupBy, r.Period, enabled)
		if err != nil {
			return fmt.Errorf("insert report %s: %w", r.Name, err)
		}
		result.ReportsCreated++
		return nil
	}

	for _, c := range data.Counters {
		cid := strings.TrimSpace(c.CounterID.String())
		dbID := counterIDToDBID[cid]
		for _, r := range c.Reports {
			if err := applyReport(dbID, r); err != nil {
				return nil, err
			}
		}
	}
	for _, fr := range data.Reports {
		cid := strings.TrimSpace(fr.CounterID.String())
		dbID, err := ensureCounterDBID(cid)
		if err != nil {
			return nil, fmt.Errorf("отчёт %q: %w", fr.Name, err)
		}
		if err := applyReport(dbID, fr.ExportReport); err != nil {
			return nil, err
		}
	}

	// Apply alert actions
	for _, a := range data.AlertActions {
		if !replace {
			var existingID int64
			var scanErr error
			switch a.Type {
			case "telegram":
				scanErr = tx.QueryRowContext(ctx, `SELECT id FROM alert_actions WHERE type = 'telegram' AND (chat_id = ? OR target = ?)`, nullInt64(a.ChatID), a.Target).Scan(&existingID)
			case "vkteams":
				scanErr = tx.QueryRowContext(ctx, `SELECT id FROM alert_actions WHERE type = 'vkteams' AND target = ?`, a.Target).Scan(&existingID)
			case "webhook":
				scanErr = tx.QueryRowContext(ctx, `SELECT id FROM alert_actions WHERE type = 'webhook' AND url = ?`, a.URL).Scan(&existingID)
			}
			if scanErr == nil {
				if a.Name != "" {
					_, _ = tx.ExecContext(ctx, `UPDATE alert_actions SET name = ? WHERE id = ?`, a.Name, existingID)
				}
				result.ActionsUpdated++
				continue
			} else if scanErr != sql.ErrNoRows {
				return nil, fmt.Errorf("query alert action: %w", scanErr)
			}
		}

		_, err := tx.ExecContext(ctx,
			`INSERT INTO alert_actions (name, type, chat_id, target, url) VALUES (?, ?, ?, ?, ?)`,
			a.Name, a.Type, nullInt64(a.ChatID), a.Target, a.URL)
		if err != nil {
			return nil, fmt.Errorf("insert alert action: %w", err)
		}
		result.ActionsCreated++
	}

	// Apply schedule
	if data.Schedule != "" {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
			SettingReportSchedule, data.Schedule)
		if err != nil {
			return nil, fmt.Errorf("set report schedule: %w", err)
		}
		result.ScheduleUpdated = true
		result.Schedule = data.Schedule
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit import tx: %w", err)
	}

	return result, nil
}
