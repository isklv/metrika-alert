package model

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

type dummyValidator struct {
	invalidMetric string
}

func (d *dummyValidator) ValidateMetric(m string) error {
	if m == d.invalidMetric {
		return fmt.Errorf("bad metric %s", m)
	}
	return nil
}

func (d *dummyValidator) ValidateSchedule(s string) error {
	if s == "invalid_schedule" {
		return fmt.Errorf("bad schedule")
	}
	return nil
}

func (d *dummyValidator) ValidatePeriod(p string) error {
	if p == "bad_period" {
		return fmt.Errorf("bad period")
	}
	return nil
}

func setupTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestExportAndImportRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// 1. Populate DB with counter, trigger, report, action, schedule
	c := &Counter{
		Name:         "Store",
		CounterID:    "12345678",
		OAuthToken:   "token_secret",
		PollInterval: 15,
	}
	if err := db.CreateCounter(ctx, c); err != nil {
		t.Fatalf("CreateCounter: %v", err)
	}

	tr := &Trigger{
		CounterID:     c.ID,
		Name:          "Drop alert",
		Metric:        "visits",
		Direction:     "drop",
		DeviationPct:  30,
		MinBaseline:   20,
		BaselineWeeks: 4,
		URLFilter:     "/checkout",
		URLMatch:      "contains",
		Cooldown:      120,
		Enabled:       true,
	}
	if err := db.CreateTrigger(ctx, tr); err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}

	rp := &Report{
		CounterID: c.ID,
		Name:      "Checkout report",
		URLFilter: "/checkout",
		URLMatch:  "contains",
		GoalIDs:   []int64{42, 77},
		GroupBy:   "url",
		Period:    "yesterday",
		Enabled:   true,
	}
	if err := db.CreateReport(ctx, rp); err != nil {
		t.Fatalf("CreateReport: %v", err)
	}

	cid := int64(-100123456)
	act := &AlertAction{
		Name:   "telegram--100123456",
		Type:   "telegram",
		ChatID: &cid,
		Target: "-100123456",
	}
	if err := db.CreateAlertAction(ctx, act); err != nil {
		t.Fatalf("CreateAlertAction: %v", err)
	}

	if err := db.SetSetting(ctx, SettingReportSchedule, "10:00"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	// 2. Export full
	exp, err := db.Export(ctx, 0, true)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	if len(exp.Counters) != 1 {
		t.Fatalf("expected 1 counter, got %d", len(exp.Counters))
	}
	if exp.Counters[0].OAuthToken != "token_secret" {
		t.Errorf("expected token_secret, got %q", exp.Counters[0].OAuthToken)
	}
	if len(exp.Counters[0].Triggers) != 1 {
		t.Fatalf("expected 1 trigger, got %d", len(exp.Counters[0].Triggers))
	}
	if len(exp.Counters[0].Reports) != 1 {
		t.Fatalf("expected 1 report, got %d", len(exp.Counters[0].Reports))
	}
	if len(exp.AlertActions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(exp.AlertActions))
	}
	if exp.Schedule != "10:00" {
		t.Errorf("expected schedule 10:00, got %q", exp.Schedule)
	}

	// 3. Export safe (no tokens)
	expSafe, err := db.Export(ctx, 0, false)
	if err != nil {
		t.Fatalf("Export safe: %v", err)
	}
	if expSafe.Counters[0].OAuthToken != "" {
		t.Errorf("expected empty token in safe mode, got %q", expSafe.Counters[0].OAuthToken)
	}

	// 4. Import into a fresh DB
	db2 := setupTestDB(t)
	res, err := db2.Import(ctx, exp, false, &dummyValidator{})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	if res.CountersCreated != 1 || res.TriggersCreated != 1 || res.ReportsCreated != 1 || res.ActionsCreated != 1 || !res.ScheduleUpdated {
		t.Fatalf("unexpected import result: %+v", res)
	}

	// Verify data in db2
	c2List, err := db2.ListCounters(ctx)
	if err != nil || len(c2List) != 1 {
		t.Fatalf("ListCounters db2: %v, count=%d", err, len(c2List))
	}
	if c2List[0].Name != "Store" || c2List[0].OAuthToken != "token_secret" {
		t.Errorf("db2 counter mismatch: %+v", c2List[0])
	}

	tr2List, err := db2.ListTriggers(ctx, c2List[0].ID)
	if err != nil || len(tr2List) != 1 {
		t.Fatalf("ListTriggers db2: %v, count=%d", err, len(tr2List))
	}
	if tr2List[0].Name != "Drop alert" || tr2List[0].DeviationPct != 30 {
		t.Errorf("db2 trigger mismatch: %+v", tr2List[0])
	}

	rp2List, err := db2.ListReports(ctx, c2List[0].ID)
	if err != nil || len(rp2List) != 1 {
		t.Fatalf("ListReports db2: %v, count=%d", err, len(rp2List))
	}
	if rp2List[0].Name != "Checkout report" || len(rp2List[0].GoalIDs) != 2 {
		t.Errorf("db2 report mismatch: %+v", rp2List[0])
	}

	// 5. Test Merge (update existing counter name and trigger deviation)
	exp.Counters[0].Name = "Store Renamed"
	exp.Counters[0].Triggers[0].DeviationPct = 45
	exp.Counters[0].OAuthToken = "" // should preserve existing token_secret

	resMerge, err := db2.Import(ctx, exp, false, &dummyValidator{})
	if err != nil {
		t.Fatalf("Import merge: %v", err)
	}
	if resMerge.CountersUpdated != 1 || resMerge.TriggersUpdated != 1 {
		t.Errorf("expected 1 counter updated and 1 trigger updated, got %+v", resMerge)
	}

	cUpdated, _ := db2.GetCounter(ctx, c2List[0].ID)
	if cUpdated.Name != "Store Renamed" || cUpdated.OAuthToken != "token_secret" {
		t.Errorf("merge counter update failed: %+v", cUpdated)
	}
	trUpdated, _ := db2.GetTrigger(ctx, tr2List[0].ID)
	if trUpdated.DeviationPct != 45 {
		t.Errorf("merge trigger update failed: %+v", trUpdated)
	}

	// 6. Test Replace mode
	expReplace := &ExportData{
		Counters: []ExportCounter{
			{
				Name:       "Brand New Counter",
				CounterID:  "87654321",
				OAuthToken: "new_token",
			},
		},
	}
	resReplace, err := db2.Import(ctx, expReplace, true, &dummyValidator{})
	if err != nil {
		t.Fatalf("Import replace: %v", err)
	}
	if resReplace.CountersCreated != 1 {
		t.Errorf("expected 1 counter created, got %+v", resReplace)
	}
	countersAfterReplace, _ := db2.ListCounters(ctx)
	if len(countersAfterReplace) != 1 || countersAfterReplace[0].CounterID != "87654321" {
		t.Errorf("replace failed: %+v", countersAfterReplace)
	}
}

func TestUnmarshalExportData(t *testing.T) {
	// JSON with numeric counter_id and markdown fence
	rawJSON := "```json\n" + `{
  "version": 1,
  "schedule": "10:00",
  "counters": [
    {
      "name": "Shop",
      "counter_id": 99887766,
      "poll_interval_minutes": 30,
      "triggers": [
        {
          "name": "Traffic drop",
          "metric": "visits",
          "direction": "drop",
          "deviation_percent": 35
        }
      ]
    }
  ]
}` + "\n```"

	data, err := UnmarshalExportData([]byte(rawJSON))
	if err != nil {
		t.Fatalf("UnmarshalExportData JSON: %v", err)
	}
	if len(data.Counters) != 1 || data.Counters[0].CounterID.String() != "99887766" {
		t.Errorf("unexpected parsed data: %+v", data)
	}

	// YAML format
	rawYAML := `
version: 1
schedule: 6h
counters:
  - name: Blog
    counter_id: "112233"
    triggers:
      - name: Spikes
        metric: pageviews
        direction: rise
        deviation_percent: 50
`
	dataYAML, err := UnmarshalExportData([]byte(rawYAML))
	if err != nil {
		t.Fatalf("UnmarshalExportData YAML: %v", err)
	}
	if dataYAML.Schedule != "6h" || len(dataYAML.Counters) != 1 || dataYAML.Counters[0].CounterID.String() != "112233" {
		t.Errorf("unexpected parsed YAML: %+v", dataYAML)
	}
}

func TestImportValidationErrors(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	validator := &dummyValidator{invalidMetric: "unknown_metric"}

	tests := []struct {
		name    string
		data    *ExportData
		wantErr string
	}{
		{
			name:    "empty data",
			data:    &ExportData{},
			wantErr: "в данных нет настроек",
		},
		{
			name: "non-numeric counter id",
			data: &ExportData{
				Counters: []ExportCounter{
					{Name: "Bad", CounterID: "abc"},
				},
			},
			wantErr: "должен быть числом",
		},
		{
			name: "invalid trigger metric",
			data: &ExportData{
				Counters: []ExportCounter{
					{
						Name:      "Good",
						CounterID: "12345",
						Triggers: []ExportTrigger{
							{Name: "Rule", Metric: "unknown_metric", DeviationPct: 40},
						},
					},
				},
			},
			wantErr: "bad metric",
		},
		{
			name: "invalid regex in url filter",
			data: &ExportData{
				Counters: []ExportCounter{
					{
						Name:      "Good",
						CounterID: "12345",
						Triggers: []ExportTrigger{
							{Name: "Rule", Metric: "visits", DeviationPct: 40, URLFilter: "[a-z", URLMatch: "regexp"},
						},
					},
				},
			},
			wantErr: "неверное регулярное выражение",
		},
		{
			name: "invalid deviation pct",
			data: &ExportData{
				Counters: []ExportCounter{
					{
						Name:      "Good",
						CounterID: "12345",
						Triggers: []ExportTrigger{
							{Name: "Rule", Metric: "visits", DeviationPct: 150},
						},
					},
				},
			},
			wantErr: "порог",
		},
		{
			name: "invalid action type",
			data: &ExportData{
				AlertActions: []ExportAction{
					{Type: "slack", Target: "channel"},
				},
			},
			wantErr: "неизвестный тип",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.Import(ctx, tt.data, false, validator)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}
