package app

import (
	"reflect"
	"testing"
	"time"
)

func scheduleTime(value string) time.Time {
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		panic(err)
	}
	return at
}

func scheduleSettings() AgentSettings {
	p := testGenerationPolicy()
	p.Timezone, p.ActiveStart, p.ActiveEnd = "UTC", "09:00", "17:00"
	p.ScheduledMinPerDay, p.ScheduledMaxPerDay, p.ScheduledPostCapPerDay = 3, 3, 3
	p.MinSpacingSeconds, p.SourceMaxAgeSeconds = 3600, 7200
	return AgentSettings{AgentID: NewID(), PersonaVersion: 1, Enabled: true, Policy: p, UpdatedAt: scheduleTime("2026-09-20T08:00:00Z")}
}

func TestGenerationScheduleDST(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ date, start, end, wantStart, wantEnd string }{
		{"2026-03-08", "02:30", "04:00", "2026-03-08T07:00:00Z", "2026-03-08T08:00:00Z"},
		{"2026-03-08", "02:00", "02:45", "2026-03-08T07:00:00Z", "2026-03-08T07:00:00Z"},
		{"2026-11-01", "01:00", "02:00", "2026-11-01T05:00:00Z", "2026-11-01T07:00:00Z"},
		{"2026-11-01", "01:15", "01:45", "2026-11-01T05:15:00Z", "2026-11-01T05:45:00Z"},
	} {
		p := scheduleSettings().Policy
		p.ActiveStart, p.ActiveEnd = tt.start, tt.end
		start, end := generationWindow(tt.date, p, location)
		if !start.Equal(scheduleTime(tt.wantStart)) || !end.Equal(scheduleTime(tt.wantEnd)) {
			t.Fatalf("%+v: %v - %v", tt, start, end)
		}
	}
	// A nominal three-slot day loses one slot across the spring-forward gap.
	s := scheduleSettings()
	s.Policy.Timezone, s.Policy.ActiveStart, s.Policy.ActiveEnd = "America/New_York", "01:00", "04:00"
	s, _, err = AdvanceGenerationSchedule(s, scheduleTime("2026-03-08T05:00:00Z"), func(int64) int64 { return 0 })
	if err != nil || s.RemainingSlots != 2 {
		t.Fatalf("spring capacity: %+v, %v", s, err)
	}
}

func TestGenerationScheduleRestartAndZero(t *testing.T) {
	s := scheduleSettings()
	now := scheduleTime("2026-09-20T08:00:00Z")
	s, due, err := AdvanceGenerationSchedule(s, now, func(n int64) int64 { return n / 2 })
	if err != nil || due != nil || s.RemainingSlots != 3 || s.NextPostAt == nil {
		t.Fatalf("initialize: %+v %v %v", s, due, err)
	}
	restarted, due, err := AdvanceGenerationSchedule(s, now, func(int64) int64 { t.Fatal("rerolled persisted future slot"); return 0 })
	if err != nil || due != nil || !reflect.DeepEqual(s, restarted) {
		t.Fatalf("restart: %+v %v", restarted, err)
	}
	s = scheduleSettings()
	s.Policy.ScheduledMinPerDay = 0
	s, _, err = AdvanceGenerationSchedule(s, now, func(int64) int64 { return 0 })
	if err != nil || s.RemainingSlots != 0 || s.ScheduleDate != "2026-09-20" || s.NextPostAt != nil {
		t.Fatalf("zero: %+v %v", s, err)
	}
	_, due, err = AdvanceGenerationSchedule(s, now.Add(5*time.Hour), func(int64) int64 { t.Fatal("rerolled zero"); return 0 })
	if err != nil || due != nil {
		t.Fatal(due, err)
	}
	s, _, err = AdvanceGenerationSchedule(s, now.Add(24*time.Hour), func(n int64) int64 { return n - 1 })
	if err != nil || s.RemainingSlots != 3 || s.ScheduleDate != "2026-09-21" {
		t.Fatalf("new local day: %+v %v", s, err)
	}
}

func TestGenerationSchedulePartialDayAndSpacing(t *testing.T) {
	s := scheduleSettings()
	now := scheduleTime("2026-09-20T15:30:00.123456789Z")
	s, due, err := AdvanceGenerationSchedule(s, now, func(int64) int64 { return 0 })
	if err != nil || due == nil || !due.At.Equal(GenerationInstant(now)) || !due.ExpiresAt.Equal(due.At.Add(2*time.Hour)) || s.RemainingSlots != 1 || !s.NextPostAt.Equal(due.At.Add(time.Hour)) {
		t.Fatalf("partial day: %+v %+v %v", s, due, err)
	}
	s, due, err = AdvanceGenerationSchedule(s, scheduleTime("2026-09-20T17:00:00Z"), func(int64) int64 { return 0 })
	if err != nil || due != nil || s.NextPostAt != nil || s.RemainingSlots != 0 {
		t.Fatalf("end exclusive: %+v %+v %v", s, due, err)
	}
	// Last publication carries spacing across initialization.
	s = scheduleSettings()
	last := scheduleTime("2026-09-20T09:30:00Z")
	s.LastPublishedAt = &last
	s, due, err = AdvanceGenerationSchedule(s, scheduleTime("2026-09-20T10:00:00Z"), func(int64) int64 { return 0 })
	if err != nil || due != nil || !s.NextPostAt.Equal(last.Add(time.Hour)) {
		t.Fatalf("publication spacing: %+v %v", s, err)
	}
}

func TestGenerationScheduleDowntime(t *testing.T) {
	s := scheduleSettings()
	s.Policy.ScheduledMinPerDay, s.Policy.ScheduledMaxPerDay, s.Policy.ScheduledPostCapPerDay = 8, 8, 8
	s, _, err := AdvanceGenerationSchedule(s, scheduleTime("2026-09-20T08:00:00Z"), func(int64) int64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	now := scheduleTime("2026-09-20T14:30:00Z")
	s, due, err := AdvanceGenerationSchedule(s, now, func(int64) int64 { return 0 })
	if err != nil || due == nil || !due.At.Equal(scheduleTime("2026-09-20T14:00:00Z")) || !s.NextPostAt.Equal(now.Add(time.Hour)) || s.RemainingSlots != 2 {
		t.Fatalf("resume: %+v %+v %v", s, due, err)
	}
	_, due, err = AdvanceGenerationSchedule(s, now, nil)
	if err != nil || due != nil {
		t.Fatalf("burst: %+v %v", due, err)
	}
	// Even the maximum count is bounded, with stale slots consumed, not emitted.
	s = scheduleSettings()
	s.Policy.ScheduledMinPerDay, s.Policy.ScheduledMaxPerDay, s.Policy.ScheduledPostCapPerDay = 100, 100, 100
	s.Policy.MinSpacingSeconds = 1
	s, _, err = AdvanceGenerationSchedule(s, scheduleTime("2026-09-20T08:00:00Z"), func(int64) int64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s, due, err = AdvanceGenerationSchedule(s, now, func(int64) int64 { calls++; return 0 })
	if err != nil || due != nil || s.RemainingSlots != 0 || calls > 100 {
		t.Fatalf("bounded stale consumption: %+v %+v %v calls=%d", s, due, err, calls)
	}
}

func TestGenerationScheduleRejectsInvalidRandomness(t *testing.T) {
	for _, draw := range []func(int64) int64{nil, func(int64) int64 { return -1 }, func(n int64) int64 { return n }} {
		if _, _, err := AdvanceGenerationSchedule(scheduleSettings(), scheduleTime("2026-09-20T08:00:00Z"), draw); err == nil {
			t.Fatal("accepted invalid draw")
		}
	}
}

func TestGenerationScheduleLocalDateAndFallSpacing(t *testing.T) {
	s := scheduleSettings()
	s.Policy.Timezone = "America/New_York"
	s.Policy.ActiveStart, s.Policy.ActiveEnd = "01:00", "03:00"
	s.Policy.ScheduledMinPerDay, s.Policy.ScheduledMaxPerDay = 2, 2
	// UTC November 1 is still October 31 locally. An exhausted local day must
	// not reroll until the local calendar changes.
	s.ScheduleDate = "2026-10-31"
	s, due, err := AdvanceGenerationSchedule(s, scheduleTime("2026-11-01T02:00:00Z"), nil)
	if err != nil || due != nil || s.ScheduleDate != "2026-10-31" {
		t.Fatalf("UTC rollover: %+v %v", s, err)
	}
	s, due, err = AdvanceGenerationSchedule(s, scheduleTime("2026-11-01T05:00:00Z"), func(int64) int64 { return 0 })
	if err != nil || due == nil || s.RemainingSlots != 1 || !s.NextPostAt.Equal(scheduleTime("2026-11-01T06:00:00Z")) {
		t.Fatalf("fall spacing: %+v %+v %v", s, due, err)
	}
	// Both instants display 01:00, but are one real hour apart.
	s, due, err = AdvanceGenerationSchedule(s, *s.NextPostAt, nil)
	if err != nil || due == nil || s.RemainingSlots != 0 {
		t.Fatalf("repeated hour: %+v %+v %v", s, due, err)
	}
	_, due, err = AdvanceGenerationSchedule(s, scheduleTime("2026-11-01T04:00:00Z"), nil)
	if err != nil || due != nil {
		t.Fatalf("backwards clock rerolled: %+v %v", due, err)
	}
}
