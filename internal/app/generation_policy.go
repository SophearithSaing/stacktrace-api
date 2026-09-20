package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// GenerationPolicy is persisted configuration, not an HTTP DTO. All fields are
// required in JSON, including explicit zero probabilities and quotas. Zero caps
// disable that activity; they never mean unlimited. Durations are whole seconds.
type GenerationPolicy struct {
	Version                    int    `json:"version"`
	Timezone                   string `json:"timezone"`
	ActiveStart                string `json:"active_start"`
	ActiveEnd                  string `json:"active_end"`
	ScheduledMinPerDay         int    `json:"scheduled_min_per_day"`
	ScheduledMaxPerDay         int    `json:"scheduled_max_per_day"`
	MinSpacingSeconds          int    `json:"min_spacing_seconds"`
	ResponseMinDelaySeconds    int    `json:"response_min_delay_seconds"`
	ResponseMaxDelaySeconds    int    `json:"response_max_delay_seconds"`
	SourceMaxAgeSeconds        int    `json:"source_max_age_seconds"`
	ReplyProbabilityBPS        int    `json:"reply_probability_bps"`
	RepostProbabilityBPS       int    `json:"repost_probability_bps"`
	QuoteProbabilityBPS        int    `json:"quote_probability_bps"`
	HumanPostProbabilityBPS    int    `json:"human_post_probability_bps"`
	ContinuationProbabilityBPS int    `json:"continuation_probability_bps"`
	CooldownSeconds            int    `json:"cooldown_seconds"`
	ScheduledPostCapPerDay     int    `json:"scheduled_post_cap_per_day"`
	ReplyCapPerDay             int    `json:"reply_cap_per_day"`
	ReplyCapPerConversation    int    `json:"reply_cap_per_conversation"`
	MaxAgentsPerTrigger        int    `json:"max_agents_per_trigger"`
	HumanTriggerCapPerWindow   int    `json:"human_trigger_cap_per_window"`
	HumanTriggerWindowSeconds  int    `json:"human_trigger_window_seconds"`
	MaxChainDepth              int    `json:"max_chain_depth"`
	MaxChainJobs               int    `json:"max_chain_jobs"`
	DailyTokenBudget           int64  `json:"daily_token_budget"`
}

// DecodeGenerationPolicy rejects unknown, duplicate, missing and null fields as
// well as unsupported versions. Its bounded, flat schema deliberately has no
// extension bag or implicit defaults.
func DecodeGenerationPolicy(data []byte) (GenerationPolicy, error) {
	var policy GenerationPolicy
	if len(data) > 8192 {
		return policy, fmt.Errorf("generation policy exceeds 8192 bytes")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return policy, fmt.Errorf("generation policy must be an object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return policy, fmt.Errorf("invalid policy field: %w", err)
		}
		name, ok := token.(string)
		if !ok {
			return policy, fmt.Errorf("invalid policy field name")
		}
		if _, exists := fields[name]; exists {
			return policy, fmt.Errorf("duplicate policy field %q", name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return policy, fmt.Errorf("invalid policy field %q: %w", name, err)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return policy, fmt.Errorf("null policy field %q", name)
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return policy, fmt.Errorf("invalid policy object: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return policy, fmt.Errorf("trailing policy data")
	}
	required := []string{
		"version", "timezone", "active_start", "active_end", "scheduled_min_per_day", "scheduled_max_per_day",
		"min_spacing_seconds", "response_min_delay_seconds", "response_max_delay_seconds", "source_max_age_seconds",
		"reply_probability_bps", "repost_probability_bps", "quote_probability_bps", "human_post_probability_bps", "continuation_probability_bps",
		"cooldown_seconds", "scheduled_post_cap_per_day", "reply_cap_per_day", "reply_cap_per_conversation", "max_agents_per_trigger",
		"human_trigger_cap_per_window", "human_trigger_window_seconds", "max_chain_depth", "max_chain_jobs", "daily_token_budget",
	}
	for _, name := range required {
		if _, exists := fields[name]; !exists {
			return policy, fmt.Errorf("missing policy field %q", name)
		}
	}
	if len(fields) != len(required) {
		return policy, fmt.Errorf("unknown policy field")
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return GenerationPolicy{}, fmt.Errorf("invalid generation policy: %w", err)
	}
	if err := policy.Validate(); err != nil {
		return GenerationPolicy{}, err
	}
	return policy, nil
}

func (p GenerationPolicy) Validate() error {
	if p.Version != 1 {
		return fmt.Errorf("unsupported generation policy version")
	}
	// Local depends on host configuration and is not a portable schedule zone.
	if p.Timezone == "" || p.Timezone == "Local" {
		return fmt.Errorf("timezone must be an explicit IANA location")
	}
	if _, err := time.LoadLocation(p.Timezone); err != nil {
		return fmt.Errorf("invalid policy timezone")
	}
	start, err := time.Parse("15:04", p.ActiveStart)
	if err != nil || start.Format("15:04") != p.ActiveStart {
		return fmt.Errorf("active_start must be HH:MM")
	}
	end, err := time.Parse("15:04", p.ActiveEnd)
	if err != nil || end.Format("15:04") != p.ActiveEnd || !end.After(start) {
		return fmt.Errorf("active_end must be HH:MM after active_start; overnight windows are unsupported")
	}
	for _, bound := range []struct {
		name            string
		value, min, max int64
	}{
		{"scheduled_min_per_day", int64(p.ScheduledMinPerDay), 0, 100},
		{"scheduled_max_per_day", int64(p.ScheduledMaxPerDay), 0, 100},
		{"min_spacing_seconds", int64(p.MinSpacingSeconds), 1, 86400},
		{"response_min_delay_seconds", int64(p.ResponseMinDelaySeconds), 0, 86400},
		{"response_max_delay_seconds", int64(p.ResponseMaxDelaySeconds), 0, 86400},
		{"source_max_age_seconds", int64(p.SourceMaxAgeSeconds), 1, 604800},
		{"reply_probability_bps", int64(p.ReplyProbabilityBPS), 0, 10000},
		{"repost_probability_bps", int64(p.RepostProbabilityBPS), 0, 10000},
		{"quote_probability_bps", int64(p.QuoteProbabilityBPS), 0, 10000},
		{"human_post_probability_bps", int64(p.HumanPostProbabilityBPS), 0, 10000},
		{"continuation_probability_bps", int64(p.ContinuationProbabilityBPS), 0, 10000},
		{"cooldown_seconds", int64(p.CooldownSeconds), 1, 604800},
		{"scheduled_post_cap_per_day", int64(p.ScheduledPostCapPerDay), 0, 100},
		{"reply_cap_per_day", int64(p.ReplyCapPerDay), 0, 1000},
		{"reply_cap_per_conversation", int64(p.ReplyCapPerConversation), 0, 100},
		{"max_agents_per_trigger", int64(p.MaxAgentsPerTrigger), 0, 10},
		{"human_trigger_cap_per_window", int64(p.HumanTriggerCapPerWindow), 0, 1000},
		{"human_trigger_window_seconds", int64(p.HumanTriggerWindowSeconds), 1, 86400},
		{"max_chain_depth", int64(p.MaxChainDepth), 0, 10},
		{"max_chain_jobs", int64(p.MaxChainJobs), 1, 100},
		{"daily_token_budget", p.DailyTokenBudget, 0, 10000000},
	} {
		if bound.value < bound.min || bound.value > bound.max {
			return fmt.Errorf("%s must be between %d and %d", bound.name, bound.min, bound.max)
		}
	}
	if p.ScheduledMinPerDay > p.ScheduledMaxPerDay || p.ScheduledMaxPerDay > p.ScheduledPostCapPerDay {
		return fmt.Errorf("scheduled range exceeds daily cap or is reversed")
	}
	// The end of the window is exclusive. This checks nominal wall-clock
	// feasibility only; a later scheduler must handle DST and missed slots.
	if p.ScheduledMaxPerDay > 1 && time.Duration(p.ScheduledMaxPerDay-1)*time.Duration(p.MinSpacingSeconds)*time.Second >= end.Sub(start) {
		return fmt.Errorf("scheduled spacing does not fit active window")
	}
	if p.ResponseMinDelaySeconds > p.ResponseMaxDelaySeconds || p.ResponseMaxDelaySeconds >= p.SourceMaxAgeSeconds {
		return fmt.Errorf("response delays must be ordered and less than source max age")
	}
	if p.MaxChainDepth >= p.MaxChainJobs {
		return fmt.Errorf("chain jobs must include the root and every depth")
	}
	return nil
}
