package engine

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule says when periodic reports are sent.
//
// A daily report needs a time of day, not just an interval: "every 24 hours"
// is measured from whenever the process last started, so the report drifts to a
// new hour after every restart and lands at three in the morning as often as
// not.
type Schedule struct {
	// Kind is "off", "every" or "daily".
	Kind string
	// Every is the gap between reports, for Kind "every".
	Every time.Duration
	// Hour and Minute are the time of day, for Kind "daily".
	Hour   int
	Minute int
}

// Schedule kinds.
const (
	ScheduleOff   = "off"
	ScheduleEvery = "every"
	ScheduleDaily = "daily"
)

// Off reports whether periodic reports are disabled.
func (s Schedule) Off() bool { return s.Kind == ScheduleOff || s.Kind == "" }

// ParseSchedule reads a schedule written the way a person types it:
//
//	off      — no periodic reports
//	10:00    — every day at 10:00
//	6h       — every six hours
//	90m      — every ninety minutes
func ParseSchedule(s string) (Schedule, error) {
	s = strings.ToLower(strings.TrimSpace(s))

	switch s {
	case "", "off", "выкл", "никогда":
		return Schedule{Kind: ScheduleOff}, nil
	}

	// A time of day is the common case, so it is recognised first.
	if hour, minute, ok := parseTimeOfDay(s); ok {
		return Schedule{Kind: ScheduleDaily, Hour: hour, Minute: minute}, nil
	}

	if every, err := parseEvery(s); err == nil {
		return Schedule{Kind: ScheduleEvery, Every: every}, nil
	}

	return Schedule{}, fmt.Errorf("не понял расписание %q: нужно время суток (`10:00`), интервал (`6h`) или `off`", s)
}

// parseTimeOfDay reads "10:00" or "9:30".
func parseTimeOfDay(s string) (hour, minute int, ok bool) {
	h, m, found := strings.Cut(s, ":")
	if !found {
		return 0, 0, false
	}
	hour, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, false
	}
	minute, err = strconv.Atoi(strings.TrimSpace(m))
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, false
	}
	return hour, minute, true
}

// minReportInterval keeps a mistyped interval from hammering the API.
const minReportInterval = time.Hour

// parseEvery reads "6h", "90m" or a bare number of hours.
func parseEvery(s string) (time.Duration, error) {
	// A bare number means hours, which is what the config key has always meant.
	if n, err := strconv.Atoi(s); err == nil {
		s = strconv.Itoa(n) + "h"
	}

	every, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if every < minReportInterval {
		return 0, fmt.Errorf("интервал меньше %s", minReportInterval)
	}
	return every, nil
}

// String renders a schedule in the same form ParseSchedule reads.
func (s Schedule) String() string {
	switch s.Kind {
	case ScheduleDaily:
		return fmt.Sprintf("%02d:%02d", s.Hour, s.Minute)
	case ScheduleEvery:
		return s.Every.String()
	default:
		return ScheduleOff
	}
}

// Describe renders the schedule for a person.
func (s Schedule) Describe() string {
	switch s.Kind {
	case ScheduleDaily:
		return fmt.Sprintf("каждый день в %02d:%02d", s.Hour, s.Minute)
	case ScheduleEvery:
		if s.Every%time.Hour == 0 {
			return fmt.Sprintf("каждые %d ч", int(s.Every.Hours()))
		}
		return "каждые " + s.Every.String()
	default:
		return "отключены"
	}
}

// Next returns when the report after `last` is due. A zero `last` means the
// service has not reported yet.
func (s Schedule) Next(last, now time.Time) (time.Time, bool) {
	switch s.Kind {
	case ScheduleEvery:
		if last.IsZero() {
			return now, true
		}
		return last.Add(s.Every), true

	case ScheduleDaily:
		today := time.Date(now.Year(), now.Month(), now.Day(), s.Hour, s.Minute, 0, 0, now.Location())
		// A first run must not fire immediately at an arbitrary moment; it
		// waits for the configured time, today if it is still ahead.
		if last.IsZero() {
			if !today.After(now) {
				return today.AddDate(0, 0, 1), true
			}
			return today, true
		}
		if last.Before(today) {
			return today, true
		}
		return today.AddDate(0, 0, 1), true

	default:
		return time.Time{}, false
	}
}

// Due reports whether a report should be sent now.
func (s Schedule) Due(last, now time.Time) bool {
	next, ok := s.Next(last, now)
	return ok && !next.After(now)
}
