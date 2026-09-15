package engine

import (
	"testing"
	"time"
)

func TestParseSchedule(t *testing.T) {
	tests := []struct {
		in   string
		want Schedule
	}{
		{"10:00", Schedule{Kind: ScheduleDaily, Hour: 10}},
		{"09:30", Schedule{Kind: ScheduleDaily, Hour: 9, Minute: 30}},
		{"9:30", Schedule{Kind: ScheduleDaily, Hour: 9, Minute: 30}},
		{"23:59", Schedule{Kind: ScheduleDaily, Hour: 23, Minute: 59}},
		{"00:00", Schedule{Kind: ScheduleDaily}},
		{"6h", Schedule{Kind: ScheduleEvery, Every: 6 * time.Hour}},
		{"24h", Schedule{Kind: ScheduleEvery, Every: 24 * time.Hour}},
		{"90m", Schedule{Kind: ScheduleEvery, Every: 90 * time.Minute}},
		// A bare number means hours, as the config key always has.
		{"6", Schedule{Kind: ScheduleEvery, Every: 6 * time.Hour}},
		{"off", Schedule{Kind: ScheduleOff}},
		{"OFF", Schedule{Kind: ScheduleOff}},
		{"выкл", Schedule{Kind: ScheduleOff}},
		{"", Schedule{Kind: ScheduleOff}},
	}
	for _, tc := range tests {
		got, err := ParseSchedule(tc.in)
		if err != nil {
			t.Errorf("ParseSchedule(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseSchedule(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestParseScheduleRejectsNonsense(t *testing.T) {
	for _, in := range []string{
		"25:00", "10:60", "-1:00", "завтра", "10:00:00",
		// Below the floor that keeps a typo from hammering the API.
		"5m", "30s",
	} {
		if got, err := ParseSchedule(in); err == nil {
			t.Errorf("ParseSchedule(%q) was accepted as %+v", in, got)
		}
	}
}

// A schedule survives a round trip through storage.
func TestScheduleRoundTrips(t *testing.T) {
	for _, in := range []string{"10:00", "09:30", "6h", "off"} {
		parsed, err := ParseSchedule(in)
		if err != nil {
			t.Fatalf("ParseSchedule(%q): %v", in, err)
		}
		again, err := ParseSchedule(parsed.String())
		if err != nil {
			t.Fatalf("ParseSchedule(%q): %v", parsed.String(), err)
		}
		if again != parsed {
			t.Errorf("%q did not survive a round trip: %+v vs %+v", in, parsed, again)
		}
	}
}

// The reason a daily schedule is a time of day and not a 24-hour interval: an
// interval is measured from the last run, so it drifts with every restart.
func TestDailyScheduleHoldsItsTimeOfDay(t *testing.T) {
	daily := Schedule{Kind: ScheduleDaily, Hour: 10}

	// Reported yesterday at 10:00; today's run is due at 10:00, not 24 hours
	// after whenever the process happened to start.
	last := time.Date(2026, 9, 14, 10, 0, 0, 0, time.Local)
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.Local)

	if !daily.Due(last, now) {
		t.Error("the daily report was not due at its own time")
	}

	next, ok := daily.Next(last, now)
	if !ok || next.Hour() != 10 || next.Minute() != 0 {
		t.Errorf("next run = %v, want 10:00", next)
	}
}

func TestDailyScheduleWaitsUntilItsTime(t *testing.T) {
	daily := Schedule{Kind: ScheduleDaily, Hour: 10}
	last := time.Date(2026, 9, 14, 10, 0, 0, 0, time.Local)

	// 09:59 is not yet time.
	if daily.Due(last, time.Date(2026, 9, 15, 9, 59, 0, 0, time.Local)) {
		t.Error("fired a minute early")
	}
	// 10:00 is.
	if !daily.Due(last, time.Date(2026, 9, 15, 10, 0, 0, 0, time.Local)) {
		t.Error("did not fire at its time")
	}
}

// Once today's report has gone out, the rest of the day must stay quiet.
func TestDailyScheduleFiresOncePerDay(t *testing.T) {
	daily := Schedule{Kind: ScheduleDaily, Hour: 10}
	last := time.Date(2026, 9, 15, 10, 0, 0, 0, time.Local)

	for _, hour := range []int{10, 11, 15, 23} {
		now := time.Date(2026, 9, 15, hour, 30, 0, 0, time.Local)
		if daily.Due(last, now) {
			t.Errorf("fired again at %02d:30 on the same day", hour)
		}
	}
	// Tomorrow it is due again.
	if !daily.Due(last, time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)) {
		t.Error("did not fire the next day")
	}
}

// A service that has never reported must not fire a daily report the instant it
// starts — that would land at an arbitrary time and defeat the whole point.
func TestDailyScheduleDoesNotFireOnFirstStart(t *testing.T) {
	daily := Schedule{Kind: ScheduleDaily, Hour: 10}
	var never time.Time

	// Started at 15:00; the next report is tomorrow at 10:00.
	now := time.Date(2026, 9, 15, 15, 0, 0, 0, time.Local)
	if daily.Due(never, now) {
		t.Error("fired immediately on first start")
	}
	next, _ := daily.Next(never, now)
	if next.Day() != 16 || next.Hour() != 10 {
		t.Errorf("next run = %v, want the 16th at 10:00", next)
	}

	// Started at 08:00; today's 10:00 is still ahead.
	morning := time.Date(2026, 9, 15, 8, 0, 0, 0, time.Local)
	next, _ = daily.Next(never, morning)
	if next.Day() != 15 || next.Hour() != 10 {
		t.Errorf("next run = %v, want the 15th at 10:00", next)
	}
}

func TestEveryScheduleCountsFromTheLastRun(t *testing.T) {
	every := Schedule{Kind: ScheduleEvery, Every: 6 * time.Hour}
	last := time.Date(2026, 9, 15, 10, 0, 0, 0, time.Local)

	if every.Due(last, time.Date(2026, 9, 15, 15, 59, 0, 0, time.Local)) {
		t.Error("fired before the interval elapsed")
	}
	if !every.Due(last, time.Date(2026, 9, 15, 16, 0, 0, 0, time.Local)) {
		t.Error("did not fire once the interval elapsed")
	}
}

// An interval schedule reports as soon as the service starts; there is no time
// of day to wait for.
func TestEveryScheduleFiresOnFirstStart(t *testing.T) {
	every := Schedule{Kind: ScheduleEvery, Every: 6 * time.Hour}
	if !every.Due(time.Time{}, time.Now()) {
		t.Error("did not report on first start")
	}
}

func TestOffScheduleNeverFires(t *testing.T) {
	off := Schedule{Kind: ScheduleOff}
	if off.Due(time.Time{}, time.Now()) {
		t.Error("a disabled schedule fired")
	}
	if _, ok := off.Next(time.Time{}, time.Now()); ok {
		t.Error("a disabled schedule reported a next run")
	}
	if !off.Off() {
		t.Error("Off() is false for a disabled schedule")
	}
	if !(Schedule{}).Off() {
		t.Error("a zero Schedule should count as disabled")
	}
}

func TestScheduleDescribe(t *testing.T) {
	tests := map[string]Schedule{
		"каждый день в 10:00": {Kind: ScheduleDaily, Hour: 10},
		"каждый день в 09:30": {Kind: ScheduleDaily, Hour: 9, Minute: 30},
		"каждые 6 ч":          {Kind: ScheduleEvery, Every: 6 * time.Hour},
		"отключены":           {Kind: ScheduleOff},
	}
	for want, schedule := range tests {
		if got := schedule.Describe(); got != want {
			t.Errorf("Describe() = %q, want %q", got, want)
		}
	}
}
