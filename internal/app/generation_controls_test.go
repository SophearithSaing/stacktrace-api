package app

import (
	"reflect"
	"testing"
	"time"
)

func controlFixture(t *testing.T) (AgentSettings, GenerationPolicy, time.Time) {
	t.Helper()
	policy := GenerationPolicy{Version: 1, Timezone: "UTC", ActiveStart: "09:00", ActiveEnd: "17:00",
		ScheduledMinPerDay: 1, ScheduledMaxPerDay: 2, MinSpacingSeconds: 3600,
		ResponseMinDelaySeconds: 60, ResponseMaxDelaySeconds: 300, SourceMaxAgeSeconds: 3600,
		ReplyProbabilityBPS: 2500, RepostProbabilityBPS: 1000, QuoteProbabilityBPS: 2000,
		HumanPostProbabilityBPS: 500, ContinuationProbabilityBPS: 500, CooldownSeconds: 1800,
		ScheduledPostCapPerDay: 2, ReplyCapPerDay: 10, ReplyCapPerConversation: 2,
		MaxAgentsPerTrigger: 2, HumanTriggerCapPerWindow: 5, HumanTriggerWindowSeconds: 3600,
		MaxChainDepth: 2, MaxChainJobs: 5, DailyTokenBudget: 50000}
	next := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	published := time.Date(2026, 9, 26, 9, 30, 0, 0, time.UTC)
	settings := AgentSettings{AgentID: NewID(), PersonaVersion: 2, Enabled: true, Policy: policy,
		NextPostAt: &next, ScheduleDate: "2026-09-26", RemainingSlots: 2, LastPublishedAt: &published,
		UpdatedAt: time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)}
	if err := settings.Validate(); err != nil {
		t.Fatal(err)
	}
	return settings, policy, time.Date(2026, 9, 27, 11, 0, 0, 0, time.UTC)
}

func TestChangeGenerationPolicyRetiresSampledDay(t *testing.T) {
	settings, policy, now := controlFixture(t)
	edited := policy
	edited.MinSpacingSeconds = 1800 // A scheduling-field edit retires the sampled day.
	changed, err := ChangeGenerationPolicy(settings, edited, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(changed.Policy, edited) || !changed.UpdatedAt.Equal(GenerationInstant(now)) {
		t.Fatalf("edited policy not applied: %+v", changed)
	}
	if changed.ScheduleDate != "2026-09-27" || changed.RemainingSlots != 0 || changed.NextPostAt != nil {
		t.Fatalf("scheduling edit rerolled instead of retiring: %+v", changed)
	}
	if changed.LastPublishedAt == nil || !changed.LastPublishedAt.Equal(*settings.LastPublishedAt) ||
		changed.Enabled != settings.Enabled || changed.PersonaVersion != settings.PersonaVersion ||
		changed.AgentID != settings.AgentID || changed.PauseRevision != settings.PauseRevision {
		t.Fatalf("publication or identity history changed: %+v", changed)
	}
	// The retained ceiling closes today even when the change happens on the day
	// after the last sample.
	settings, _, _ = controlFixture(t)
	settings.ScheduleDate, settings.RemainingSlots, settings.NextPostAt = "2026-09-20", 0, nil
	changed, err = ChangeGenerationPolicy(settings, edited, now)
	if err != nil {
		t.Fatal(err)
	}
	if changed.ScheduleDate != "2026-09-27" {
		t.Fatalf("stale day survived the ceiling: %+v", changed)
	}
}

func TestChangeGenerationPolicyPreservesScheduleProgress(t *testing.T) {
	settings, policy, now := controlFixture(t)
	edited := policy
	edited.ReplyCapPerDay = 5 // Non-scheduling edits keep the day's progress.
	edited.DailyTokenBudget = 12345
	changed, err := ChangeGenerationPolicy(settings, edited, now)
	if err != nil {
		t.Fatal(err)
	}
	if changed.ScheduleDate != settings.ScheduleDate || changed.RemainingSlots != settings.RemainingSlots ||
		changed.NextPostAt == nil || !changed.NextPostAt.Equal(*settings.NextPostAt) {
		t.Fatalf("non-scheduling edit reset schedule state: %+v", changed)
	}
	if !changed.LastPublishedAt.Equal(*settings.LastPublishedAt) {
		t.Fatal("publication history erased")
	}
	if changed.Policy.ReplyCapPerDay != 5 || changed.Policy.DailyTokenBudget != 12345 || changed.Policy.MinSpacingSeconds != policy.MinSpacingSeconds {
		t.Fatalf("policy replace lost fields: %+v", changed.Policy)
	}
	if err := changed.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestChangeGenerationPolicyRetainsCeilingAcrossZones(t *testing.T) {
	settings, policy, _ := controlFixture(t)
	settings.ScheduleDate, settings.NextPostAt, settings.RemainingSlots = "", nil, 0
	settings.LastPublishedAt = nil
	settings.UpdatedAt = time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	now := time.Date(2026, 9, 27, 11, 0, 0, 0, time.UTC)
	ahead := policy
	ahead.Timezone = "Pacific/Kiritimati" // Local date is already 2026-09-28.
	changed, err := ChangeGenerationPolicy(settings, ahead, now)
	if err != nil || changed.ScheduleDate != "2026-09-28" {
		t.Fatalf("zone ceiling: %+v %v", changed, err)
	}
	// Switching back cannot reroll the retained old local day: the ceiling keeps
	// the old date even when the new zone's date has caught up.
	back := ahead
	back.Timezone = "UTC"
	later := now.Add(2 * time.Hour) // 2026-09-27 13:00 UTC; Kiritimati is 2026-09-28.
	changed, err = ChangeGenerationPolicy(changed, back, later)
	if err != nil || changed.ScheduleDate != "2026-09-28" {
		t.Fatalf("backwards zone rerolled: %+v %v", changed, err)
	}
}

func TestChangeGenerationPolicyRejectsInvalidInput(t *testing.T) {
	settings, policy, now := controlFixture(t)
	before := settings
	if got, err := ChangeGenerationPolicy(before, policy, time.Time{}); err == nil || !reflect.DeepEqual(got, before) {
		t.Fatalf("zero time: %+v %v", got, err)
	}
	bad := policy
	bad.ActiveEnd = "08:00" // Before active_start.
	if got, err := ChangeGenerationPolicy(before, bad, now); err == nil || !reflect.DeepEqual(got, before) {
		t.Fatalf("invalid policy accepted: %+v %v", got, err)
	}
	bad = policy
	bad.ScheduledMaxPerDay = 12 // Exceeds the daily cap and ScheduledMaxPerDay bound.
	if got, err := ChangeGenerationPolicy(before, bad, now); err == nil || !reflect.DeepEqual(got, before) {
		t.Fatalf("cap-violating policy accepted: %+v %v", got, err)
	}
	invalid := before
	invalid.UpdatedAt = time.Time{}
	if got, err := ChangeGenerationPolicy(invalid, policy, now); err == nil || got.PauseRevision != invalid.PauseRevision {
		t.Fatalf("invalid settings accepted: %+v %v", got, err)
	}
}
