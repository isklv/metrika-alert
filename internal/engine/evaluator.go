package engine

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

// Evaluator evaluates incoming events against configured triggers and creates alerts.
type Evaluator struct {
	db    *model.DB
	alert AlertRouter // delivers alerts to configured actions
}

type AlertRouter interface {
	Alert(ctx context.Context, counterID int64, title, message string) error
}

func NewEvaluator(db *model.DB, router AlertRouter) *Evaluator {
	return &Evaluator{db: db, alert: router}
}

// Evaluate processes a batch of events against all triggers.
func (e *Evaluator) Evaluate(ctx context.Context, counterID int64, events []*model.MetrikaEvent) error {
	triggers, err := e.db.ListTriggers(ctx, counterID)
	if err != nil {
		return fmt.Errorf("list triggers: %w", err)
	}

	for _, t := range triggers {
		if !t.Enabled {
			continue
		}

		now := time.Now()
		windowStart := now.Add(-time.Duration(t.Window) * time.Minute)

		matched, err := e.matchEvents(ctx, &t, events, windowStart)
		if err != nil {
			log.Printf("evaluate trigger %d: %v", t.ID, err)
			continue
		}
		if matched < t.Threshold {
			continue
		}

		if err := e.checkCooldown(ctx, &t); err != nil {
			continue // still in cooldown
		}

		counter, err := e.db.GetCounter(ctx, counterID)
		if err != nil {
			log.Printf("get counter %d: %v", counterID, err)
			continue
		}

		title := fmt.Sprintf("🔴 Alert: %s — %s", counter.Name, t.Name)
		msg := e.renderAlert(&t, matched)

		if err := e.alert.Alert(ctx, counterID, title, msg); err != nil {
			log.Printf("deliver alert: %v", err)
		}

		// Persist the alert.
		a := &model.Alert{
			TriggerID:  t.ID,
			CounterID:  counterID,
			Title:      title,
			Message:    msg,
			EventCount: matched,
		}
		if err := e.db.CreateAlert(ctx, a); err != nil {
			log.Printf("persist alert: %v", err)
		}

		if err := e.db.RecordTriggerFire(ctx, t.ID); err != nil {
			log.Printf("record trigger fire: %v", err)
		}
	}

	return nil
}

// matchEvents counts events matching a trigger's condition within the window.
func (e *Evaluator) matchEvents(ctx context.Context, t *model.Trigger, events []*model.MetrikaEvent, windowStart time.Time) (int, error) {
	count := 0
	for _, ev := range events {
		if ev.EventTime.Before(windowStart) {
			continue
		}

		// Monitor-scoped triggers only fire for events on monitored pages.
		if t.MonitorID != nil {
			monitor, err := e.getMonitorForTrigger(ctx, t.CounterID, *t.MonitorID)
			if err != nil || monitor == nil || !pageMatches(monitor.URLPattern, ev.PageURL) {
				continue
			}
		}

		if matchCondition(t.Condition, ev) {
			count++
		}
	}
	return count, nil
}

func (e *Evaluator) getMonitorForTrigger(ctx context.Context, counterID int64, monitorID int64) (*model.PageMonitor, error) {
	monitors, err := e.db.ListMonitors(ctx, counterID)
	if err != nil {
		return nil, err
	}
	for _, m := range monitors {
		if m.ID == monitorID && m.Enabled {
			return &m, nil
		}
	}
	return nil, nil
}

func (e *Evaluator) checkCooldown(ctx context.Context, t *model.Trigger) error {
	lastFired, ok, err := e.db.TriggerFiredAt(ctx, t.ID)
	if err != nil {
		return err
	}
	if !ok || time.Since(lastFired) >= time.Duration(t.Cooldown)*time.Minute {
		return nil
	}
	return fmt.Errorf("trigger %d in cooldown until %s", t.ID, lastFired.Format(time.RFC3339))
}

func (e *Evaluator) renderAlert(t *model.Trigger, matched int) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("*Trigger:* %s\n", t.Name))
	b.WriteString(fmt.Sprintf("*Events matched:* %d (threshold: %d)\n", matched, t.Threshold))
	b.WriteString(fmt.Sprintf("*Window:* %d min | *Cooldown:* %d min\n", t.Window, t.Cooldown))
	b.WriteString(fmt.Sprintf("*Condition:* `%s`", t.Condition))
	return b.String()
}

// matchCondition evaluates a simple condition string against an event.
// Supported: "field op value" — e.g. "status_code == 500", "revenue > 10000",
// "page_url contains /checkout", "goals_id contains 42".
func matchCondition(cond string, ev *model.MetrikaEvent) bool {
	cond = strings.TrimSpace(cond)
	if cond == "" {
		return true
	}

	parts := splitCondition(cond)
	if len(parts) < 3 {
		return false
	}

	field := strings.ToLower(strings.TrimSpace(parts[0]))
	op := strings.TrimSpace(parts[1])
	value := strings.TrimSpace(parts[2])

	fieldVal := eventField(ev, field)

	switch op {
	case "==":
		return fieldVal == value
	case "!=":
		return fieldVal != value
	case "contains":
		return strings.Contains(strings.ToLower(fieldVal), strings.ToLower(value))
	case ">":
		f1, e1 := strconv.ParseFloat(fieldVal, 64)
		f2, e2 := strconv.ParseFloat(value, 64)
		return e1 == nil && e2 == nil && f1 > f2
	case "<":
		f1, e1 := strconv.ParseFloat(fieldVal, 64)
		f2, e2 := strconv.ParseFloat(value, 64)
		return e1 == nil && e2 == nil && f1 < f2
	case ">=":
		f1, e1 := strconv.ParseFloat(fieldVal, 64)
		f2, e2 := strconv.ParseFloat(value, 64)
		return e1 == nil && e2 == nil && f1 >= f2
	case "<=":
		f1, e1 := strconv.ParseFloat(fieldVal, 64)
		f2, e2 := strconv.ParseFloat(value, 64)
		return e1 == nil && e2 == nil && f1 <= f2
	default:
		return fieldVal == value
	}
}

func eventField(ev *model.MetrikaEvent, field string) string {
	switch field {
	case "page_url", "url":
		return ev.PageURL
	case "title":
		return ev.Title
	case "status_code", "status":
		return ev.Status
	case "revenue":
		return fmt.Sprintf("%.2f", ev.Revenue)
	case "order_id":
		return ev.OrderID
	case "client_id":
		return ev.ClientID
	case "user_id":
		return ev.UserID
	case "goals_id", "goals":
		return strings.Join(ev.GoalsID, ",")
	default:
		if v, ok := ev.Params[field]; ok {
			return v
		}
		return ""
	}
}

func splitCondition(cond string) []string {
	// Split on first occurrence of ==, !=, contains, >, <, >=, <=.
	for _, op := range []string{"contains", "==", "!=", ">=", "<=", ">", "<"} {
		if idx := strings.Index(cond, op); idx > 0 {
			return []string{cond[:idx], op, cond[idx+len(op):]}
		}
	}
	return []string{cond}
}

// pageMatches checks if a URL matches a glob-like pattern.
// Supports * as wildcard: "/checkout*", "*/api/*".
func pageMatches(pattern, url string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	// Simple glob: split on *, check segments.
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return url == pattern
	}

	if !strings.HasPrefix(url, parts[0]) {
		return false
	}
	remaining := url[len(parts[0]):]
	for _, part := range parts[1:] {
		idx := strings.Index(remaining, part)
		if idx < 0 {
			return false
		}
		remaining = remaining[idx+len(part):]
	}
	return true
}
