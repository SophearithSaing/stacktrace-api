package app

import (
	"testing"
	"time"
)

func TestScheduledGenerationExecutionAllowed(t *testing.T) {
	for _, test := range []struct {
		name, zone, start, end, slot, now string
		want                              bool
	}{
		{"start inclusive", "UTC", "09:00", "17:00", "2026-09-20T09:00:00Z", "2026-09-20T09:00:00Z", true},
		{"end exclusive", "UTC", "09:00", "17:00", "2026-09-20T16:00:00Z", "2026-09-20T17:00:00Z", false},
		{"tightened start", "UTC", "10:00", "17:00", "2026-09-20T09:00:00Z", "2026-09-20T10:00:00Z", false},
		{"previous day", "UTC", "09:00", "17:00", "2026-09-20T16:00:00Z", "2026-09-21T09:00:00Z", false},
		{"gap clamps forward", "America/New_York", "02:30", "04:00", "2026-03-08T07:00:00Z", "2026-03-08T07:00:00Z", true},
		{"empty gap window", "America/New_York", "02:00", "02:45", "2026-03-08T07:00:00Z", "2026-03-08T07:00:00Z", false},
		{"fold earliest start", "America/New_York", "01:15", "01:45", "2026-11-01T05:15:00Z", "2026-11-01T05:30:00Z", true},
		{"fold does not reopen", "America/New_York", "01:15", "01:45", "2026-11-01T05:15:00Z", "2026-11-01T06:30:00Z", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := scheduleSettings().Policy
			p.Timezone, p.ActiveStart, p.ActiveEnd = test.zone, test.start, test.end
			p.ScheduledMinPerDay, p.ScheduledMaxPerDay, p.SourceMaxAgeSeconds = 1, 1, 604800
			slot, now := scheduleTime(test.slot), scheduleTime(test.now)
			key, _ := ScheduledGenerationKey(slot)
			if got := ScheduledGenerationExecutionAllowed(key, p, slot, slot.Add(7*24*time.Hour), now); got != test.want {
				t.Fatalf("allowed=%v want=%v", got, test.want)
			}
		})
	}
	p := scheduleSettings().Policy
	slot := scheduleTime("2026-09-20T10:00:00Z")
	key, _ := ScheduledGenerationKey(slot)
	for _, bad := range []string{"scheduled:legacy", "2026-09-20T10:00:00Z", "scheduled:v2:2026-09-20T10:00:00Z", "scheduled:v1:2026-09-20T10:00:00.000Z", "scheduled:v1:2026-09-20T12:00:00+02:00", "scheduled:v1:2026-09-20T10:00:00.000000001Z"} {
		if ScheduledGenerationExecutionAllowed(bad, p, slot, slot.Add(time.Hour), slot) {
			t.Fatalf("accepted noncanonical key %q", bad)
		}
	}
	for _, test := range []struct {
		name                  string
		p                     GenerationPolicy
		created, expires, now time.Time
	}{
		{"slot after enqueue", p, slot.Add(-time.Second), slot.Add(time.Hour), slot},
		{"immutable expiry", p, slot, slot.Add(time.Hour), slot.Add(time.Hour)},
		{"current age expiry", p, slot, slot.Add(3 * time.Hour), slot.Add(2 * time.Hour)},
		{"zero clock", p, slot, slot.Add(time.Hour), time.Time{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if ScheduledGenerationExecutionAllowed(key, test.p, test.created, test.expires, test.now) {
				t.Fatal("accepted invalid timing")
			}
		})
	}
	p.Timezone = "not/a/timezone"
	if ScheduledGenerationExecutionAllowed(key, p, slot, slot.Add(time.Hour), slot) {
		t.Fatal("invalid policy")
	}
}
