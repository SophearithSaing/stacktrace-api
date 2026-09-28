package app

import (
	"strings"
	"testing"
)

func TestDecodeAdminPersonaStrictness(t *testing.T) {
	valid := `{"version":2,"instructions":"Discuss Go tradeoffs.","topic_tags":["go"],
		"created_at":"2026-09-28T12:00:00Z"}`
	persona, err := DecodeAdminPersona([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if persona.Version != 2 || persona.Instructions != "Discuss Go tradeoffs." || len(persona.TopicTags) != 1 {
		t.Fatalf("valid persona round trip: %+v", persona)
	}
	broken := map[string]string{
		"unknown":       `{"version":1,"instructions":"x","topic_tags":["a"],"created_at":"2026-09-28T12:00:00Z","extra":1}`,
		"duplicate":     `{"version":1,"instructions":"x","instructions":"y","topic_tags":["a"],"created_at":"2026-09-28T12:00:00Z"}`,
		"missing":       `{"version":1,"instructions":"x"}`,
		"trailing":      `{"version":1,"instructions":"x","topic_tags":["a"],"created_at":"2026-09-28T12:00:00Z"} {}`,
		"invalid tags":  `{"version":1,"instructions":"x","topic_tags":["A"],"created_at":"2026-09-28T12:00:00Z"}`,
		"empty content": `{"version":1,"instructions":"","topic_tags":[],"created_at":"2026-09-28T12:00:00Z"}`,
		"missing time":  `{"version":1,"instructions":"x","topic_tags":["a"]}`,
	}
	for name, payload := range broken {
		if _, decodeErr := DecodeAdminPersona([]byte(payload)); decodeErr == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if _, err := DecodeAdminPersona([]byte(`{"version":1,"instructions":"` + strings.Repeat("x", 65000) + `","topic_tags":["a"]}`)); err == nil {
		t.Fatal("oversized persona accepted")
	}
	if _, err := DecodeAdminPersona([]byte(`not json`)); err == nil {
		t.Fatal("non-JSON accepted")
	}
}

func TestDecodeAdminPolicyBounds(t *testing.T) {
	data := `{"version":1,"timezone":"UTC","active_start":"09:00","active_end":"17:00",
		"scheduled_min_per_day":1,"scheduled_max_per_day":2,"min_spacing_seconds":3600,"response_min_delay_seconds":60,
		"response_max_delay_seconds":300,"source_max_age_seconds":3600,"reply_probability_bps":2500,"repost_probability_bps":1000,
		"quote_probability_bps":2000,"human_post_probability_bps":500,"continuation_probability_bps":500,"cooldown_seconds":1800,
		"scheduled_post_cap_per_day":2,"reply_cap_per_day":10,"reply_cap_per_conversation":2,"max_agents_per_trigger":2,
		"human_trigger_cap_per_window":5,"human_trigger_window_seconds":3600,"max_chain_depth":2,"max_chain_jobs":5,"daily_token_budget":50000}`
	policy, err := DecodeAdminPolicy([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if policy.DailyTokenBudget != 50000 || policy.Timezone != "UTC" {
		t.Fatalf("valid policy round trip: %+v", policy)
	}
	if _, err := DecodeAdminPolicy([]byte(strings.Repeat("x", 9000))); err == nil {
		t.Fatal("oversized policy accepted")
	}
	if _, err := DecodeAdminPolicy([]byte(`{"version":1}`)); err == nil {
		t.Fatal("invalid policy accepted")
	}
}
