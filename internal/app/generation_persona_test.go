package app

import (
	"strings"
	"testing"
	"time"
)

func TestPersonaValidationAndImmutability(t *testing.T) {
	p := Persona{AgentID: NewID(), Version: 1, Instructions: "Discuss Go tradeoffs. Never claim live access.", TopicTags: []string{"go", "databases"}, CreatedAt: time.Now().UTC()}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePersonaUnchanged(p, p); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Persona){
		"id":                 func(p *Persona) { p.AgentID = "bad" },
		"version":            func(p *Persona) { p.Version = 0 },
		"blank instructions": func(p *Persona) { p.Instructions = "  " },
		"long instructions":  func(p *Persona) { p.Instructions = strings.Repeat("a", 16001) },
		"invalid encoding":   func(p *Persona) { p.Instructions = "\xff" },
		"NUL":                func(p *Persona) { p.Instructions = "a\x00b" },
		"no topics":          func(p *Persona) { p.TopicTags = nil },
		"duplicate topics":   func(p *Persona) { p.TopicTags = []string{"go", "go"} },
		"unnormalized topic": func(p *Persona) { p.TopicTags = []string{" Go "} },
		"creation":           func(p *Persona) { p.CreatedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			next := p
			change(&next)
			if err := next.Validate(); err == nil {
				t.Fatal("accepted invalid persona")
			}
		})
	}
	for _, change := range []func(*Persona){
		func(p *Persona) { p.AgentID = NewID() }, func(p *Persona) { p.Version++ },
		func(p *Persona) { p.Instructions = "Different instructions" }, func(p *Persona) { p.TopicTags = []string{"rust"} },
		func(p *Persona) { p.CreatedAt = p.CreatedAt.Add(time.Second) },
	} {
		next := p
		change(&next)
		if err := ValidatePersonaUnchanged(p, next); err == nil {
			t.Fatal("accepted persona mutation")
		}
	}
}

func TestAgentSettingsValidation(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	s := AgentSettings{AgentID: NewID(), PersonaVersion: 1, Policy: testGenerationPolicy(), UpdatedAt: now}
	if err := s.Validate(); err != nil {
		t.Fatalf("disabled unscheduled settings: %v", err)
	}
	s.Enabled, s.ScheduleDate, s.RemainingSlots, s.NextPostAt = true, "2026-09-20", 2, &now
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*AgentSettings){
		"id":               func(s *AgentSettings) { s.AgentID = "" },
		"persona":          func(s *AgentSettings) { s.PersonaVersion = 0 },
		"policy":           func(s *AgentSettings) { s.Policy.Version = 2 },
		"slots":            func(s *AgentSettings) { s.RemainingSlots = 3 },
		"no slots":         func(s *AgentSettings) { s.RemainingSlots = 0 },
		"no date":          func(s *AgentSettings) { s.ScheduleDate = "" },
		"bad date":         func(s *AgentSettings) { s.ScheduleDate = "2026-02-30" },
		"different day":    func(s *AgentSettings) { s.ScheduleDate = "2026-09-19" },
		"last publication": func(s *AgentSettings) { s.LastPublishedAt = new(time.Time) },
		"updated":          func(s *AgentSettings) { s.UpdatedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			next := s
			change(&next)
			if err := next.Validate(); err == nil {
				t.Fatal("accepted invalid settings")
			}
		})
	}
	// Pausing does not discard a sampled schedule; runtime must skip stale slots.
	s.Enabled = false
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}
