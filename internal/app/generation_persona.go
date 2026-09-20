package app

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Persona versions are immutable. Changing instructions or topics requires a
// new version, not an update. Storage must also enforce this contract.
type Persona struct {
	AgentID      ID
	Version      int
	Instructions string
	TopicTags    []string
	CreatedAt    time.Time
}

func (p Persona) Validate() error {
	if !validGenerationID(p.AgentID) || p.Version < 1 || p.Version > 2147483647 || p.CreatedAt.IsZero() {
		return fmt.Errorf("invalid persona identity or creation time")
	}
	if !validGenerationText(p.Instructions, 16000) {
		return fmt.Errorf("persona instructions must contain 1-16000 UTF-8 bytes without NUL")
	}
	if len(p.TopicTags) < 1 || len(p.TopicTags) > 20 {
		return fmt.Errorf("persona must have 1-20 topic tags")
	}
	seen := make(map[string]bool)
	for _, tag := range p.TopicTags {
		if len(tag) < 1 || len(tag) > 64 || tag != strings.ToLower(tag) || seen[tag] {
			return fmt.Errorf("persona tags must be unique lowercase ASCII letter/digit/underscore slugs of 1-64 bytes")
		}
		for i := range len(tag) {
			if !isTagCharacter(tag[i]) {
				return fmt.Errorf("persona tags must contain only ASCII letters, digits or underscores")
			}
		}
		seen[tag] = true
	}
	return nil
}

func ValidatePersonaUnchanged(before, after Persona) error {
	if err := before.Validate(); err != nil {
		return err
	}
	if err := after.Validate(); err != nil {
		return err
	}
	if before.AgentID != after.AgentID || before.Version != after.Version || before.Instructions != after.Instructions || !slices.Equal(before.TopicTags, after.TopicTags) || !before.CreatedAt.Equal(after.CreatedAt) {
		return fmt.Errorf("persona versions are immutable")
	}
	return nil
}

// AgentSettings pins a persona belonging to AgentID. The persistence boundary
// must check AccountType == AccountAgent and the composite persona reference.
// ScheduleDate is an optional YYYY-MM-DD local date, not the UTC budget day.
type AgentSettings struct {
	AgentID         ID
	PersonaVersion  int
	Enabled         bool
	Policy          GenerationPolicy
	NextPostAt      *time.Time
	ScheduleDate    string
	RemainingSlots  int
	LastPublishedAt *time.Time
	UpdatedAt       time.Time
}

func (s AgentSettings) Validate() error {
	if !validGenerationID(s.AgentID) || s.PersonaVersion < 1 || s.PersonaVersion > 2147483647 || s.UpdatedAt.IsZero() {
		return fmt.Errorf("invalid agent settings identity or update time")
	}
	if err := s.Policy.Validate(); err != nil {
		return err
	}
	if s.RemainingSlots < 0 || s.RemainingSlots > s.Policy.ScheduledMaxPerDay {
		return fmt.Errorf("remaining slots exceed policy")
	}
	if s.ScheduleDate != "" && !validGenerationDate(s.ScheduleDate) {
		return fmt.Errorf("invalid local schedule date")
	}
	if s.RemainingSlots > 0 && s.ScheduleDate == "" {
		return fmt.Errorf("remaining slots require a schedule date")
	}
	if s.NextPostAt != nil {
		if s.NextPostAt.IsZero() || s.ScheduleDate == "" || s.RemainingSlots == 0 {
			return fmt.Errorf("next post requires a dated remaining slot")
		}
		location, _ := time.LoadLocation(s.Policy.Timezone) // Already validated.
		if s.NextPostAt.In(location).Format(time.DateOnly) != s.ScheduleDate {
			return fmt.Errorf("next post must fall on the local schedule date")
		}
	}
	if s.LastPublishedAt != nil && s.LastPublishedAt.IsZero() {
		return fmt.Errorf("invalid last publication time")
	}
	return nil
}

func validGenerationID(id ID) bool {
	parsed, err := ParseID(string(id))
	return err == nil && parsed == id && id != "00000000-0000-0000-0000-000000000000"
}

func validGenerationText(value string, maxBytes int) bool {
	return len(value) <= maxBytes && strings.TrimSpace(value) != "" && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validGenerationDate(value string) bool {
	date, err := time.Parse(time.DateOnly, value)
	return err == nil && date.Format(time.DateOnly) == value && date.Year() > 0
}
