package app

import (
	"fmt"
	"time"
)

// ChangeGenerationPolicy never samples opportunities or resets publication
// history. Scheduling edits retire the remaining day under BOTH timezones. The
// retained date ceiling also prevents switching zones back to reroll an old day.
func ChangeGenerationPolicy(s AgentSettings, policy GenerationPolicy, now time.Time) (AgentSettings, error) {
	if err := s.Validate(); err != nil {
		return s, err
	}
	if err := policy.Validate(); err != nil {
		return s, err
	}
	if now.IsZero() {
		return s, fmt.Errorf("policy update requires an explicit time")
	}
	old := s.Policy
	if old.Timezone != policy.Timezone || old.ActiveStart != policy.ActiveStart || old.ActiveEnd != policy.ActiveEnd ||
		old.ScheduledMinPerDay != policy.ScheduledMinPerDay || old.ScheduledMaxPerDay != policy.ScheduledMaxPerDay ||
		old.ScheduledPostCapPerDay != policy.ScheduledPostCapPerDay || old.MinSpacingSeconds != policy.MinSpacingSeconds ||
		old.SourceMaxAgeSeconds != policy.SourceMaxAgeSeconds {
		oldLocation, _ := time.LoadLocation(old.Timezone)
		newLocation, _ := time.LoadLocation(policy.Timezone)
		s.ScheduleDate = max(s.ScheduleDate, now.In(oldLocation).Format(time.DateOnly), now.In(newLocation).Format(time.DateOnly))
		s.RemainingSlots, s.NextPostAt = 0, nil
	}
	s.Policy, s.UpdatedAt = policy, GenerationInstant(now)
	return s, s.Validate()
}
