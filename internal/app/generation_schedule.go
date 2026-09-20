package app

import (
	"fmt"
	"time"
)

// ScheduledSlot retains the original instant even when admission happens later.
// Expiry must never be recalculated from the admission/retry time.
type ScheduledSlot struct {
	At        time.Time
	ExpiresAt time.Time
}

// GenerationInstant is the precision used by PostgreSQL and stable slot keys.
func GenerationInstant(at time.Time) time.Time { return at.UTC().Truncate(time.Microsecond) }

// AdvanceGenerationSchedule is pure apart from the explicitly supplied draw
// function (the contract of rand.Int64N). Persist its returned settings together
// with any admission under the settings lock. A dated zero remaining count is a
// completed sample, not an uninitialized day. No random state is needed on restart.
// Disabled settings are left untouched. Policy edits must preserve the day's
// progress; this function never resamples a count on the same local date.
func AdvanceGenerationSchedule(s AgentSettings, now time.Time, draw func(int64) int64) (AgentSettings, *ScheduledSlot, error) {
	if err := s.Validate(); err != nil {
		return s, nil, err
	}
	if now.IsZero() {
		return s, nil, fmt.Errorf("schedule requires an explicit time")
	}
	if !s.Enabled {
		return s, nil, nil
	}
	now = GenerationInstant(now)
	location, _ := time.LoadLocation(s.Policy.Timezone)
	date := now.In(location).Format(time.DateOnly)
	if s.ScheduleDate > date {
		return s, nil, nil // A backwards clock cannot reroll a persisted day.
	}
	start, end := generationWindow(date, s.Policy, location)
	spacing := time.Duration(s.Policy.MinSpacingSeconds) * time.Second
	lower := start
	if s.LastPublishedAt != nil {
		lower = laterGenerationTime(lower, GenerationInstant(*s.LastPublishedAt).Add(spacing))
	}
	if s.ScheduleDate != date {
		count, err := generationDraw(draw, int64(s.Policy.ScheduledMaxPerDay-s.Policy.ScheduledMinPerDay+1))
		if err != nil {
			return s, nil, err
		}
		s.ScheduleDate, s.RemainingSlots, s.NextPostAt = date, s.Policy.ScheduledMinPerDay+int(count), nil
		if err := sampleGenerationSlot(&s, laterGenerationTime(lower, now), end, draw); err != nil {
			return s, nil, err
		}
	} else if s.RemainingSlots > 0 && s.NextPostAt == nil {
		return s, nil, fmt.Errorf("sampled schedule is missing its next slot")
	}
	var due *ScheduledSlot
	// Advance from each original slot, not from now, so downtime consumes the
	// elapsed opportunities. RemainingSlots bounds this loop to at most 100.
	for s.NextPostAt != nil && !s.NextPostAt.After(now) {
		at := GenerationInstant(*s.NextPostAt)
		expires := at.Add(time.Duration(s.Policy.SourceMaxAgeSeconds) * time.Second)
		if !at.Before(lower) && !now.Before(start) && now.Before(end) && now.Before(expires) {
			due = &ScheduledSlot{At: at, ExpiresAt: expires}
		}
		s.RemainingSlots--
		if err := sampleGenerationSlot(&s, at.Add(spacing), end, draw); err != nil {
			return s, nil, err
		}
	}
	// Coalesce elapsed fresh slots to one. The next opportunity is at least one
	// spacing after this pass, even if admission later rejects the returned slot.
	// This prevents repeated polling from turning downtime into a catch-up burst.
	if due != nil && s.NextPostAt != nil && s.NextPostAt.Before(now.Add(spacing)) {
		if err := sampleGenerationSlot(&s, now.Add(spacing), end, draw); err != nil {
			return s, nil, err
		}
	}
	s.UpdatedAt = now
	return s, due, nil
}

func sampleGenerationSlot(s *AgentSettings, lower, end time.Time, draw func(int64) int64) error {
	s.NextPostAt = nil
	if !lower.Before(end) {
		s.RemainingSlots = 0
	}
	if s.RemainingSlots == 0 {
		return nil
	}
	spacing := time.Duration(s.Policy.MinSpacingSeconds) * time.Second
	// A shortened DST/partial day may not fit the nominal policy count.
	capacity := int((end.Sub(lower)-time.Microsecond)/spacing) + 1
	s.RemainingSlots = min(s.RemainingSlots, capacity)
	latest := end.Add(-time.Duration(s.RemainingSlots-1) * spacing)
	offset, err := generationDraw(draw, latest.Sub(lower).Microseconds())
	if err != nil {
		return err
	}
	at := lower.Add(time.Duration(offset) * time.Microsecond)
	s.NextPostAt = &at
	return nil
}

func generationDraw(draw func(int64) int64, n int64) (int64, error) {
	if n == 1 {
		return 0, nil
	}
	if n < 1 || draw == nil {
		return 0, fmt.Errorf("schedule requires a bounded random draw")
	}
	value := draw(n)
	if value < 0 || value >= n {
		return 0, fmt.Errorf("random draw is outside its bound")
	}
	return value, nil
}

func laterGenerationTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func generationWindow(date string, policy GenerationPolicy, location *time.Location) (time.Time, time.Time) {
	return generationBoundary(date, policy.ActiveStart, location), generationBoundary(date, policy.ActiveEnd, location)
}

// Resolve wall times explicitly rather than relying on time.Date's unspecified
// DST choice. Repeated boundaries choose the earliest occurrence. Missing times
// clamp forward to the first valid wall instant (02:30 -> 03:00 in a 02-03 gap).
// Walk zone intervals, not minutes, so historical second offsets work as well.
func generationBoundary(date, clock string, location *time.Location) time.Time {
	wall, _ := time.Parse("2006-01-02 15:04", date+" "+clock)
	limit := wall.Add(48 * time.Hour)
	var best time.Time
	for cursor := wall.Add(-48 * time.Hour); cursor.Before(limit); {
		_, offset := cursor.In(location).Zone()
		_, end := cursor.In(location).ZoneBounds()
		candidate := laterGenerationTime(wall.Add(-time.Duration(offset)*time.Second), cursor)
		if (end.IsZero() || candidate.Before(end)) && (best.IsZero() || candidate.Before(best)) {
			best = candidate
		}
		if end.IsZero() {
			break
		}
		cursor = end
	}
	return best.UTC()
}
