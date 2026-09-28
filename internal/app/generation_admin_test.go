package app

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestDecodeAdminPersonaStrictness(t *testing.T) {
	agentID := NewID()
	valid := `{"version":2,"instructions":"Discuss Go tradeoffs.","topic_tags":["go"],
		"created_at":"2026-09-28T12:00:00Z"}`
	persona, err := DecodeAdminPersona([]byte(valid), agentID)
	if err != nil {
		t.Fatal(err)
	}
	if persona.AgentID != agentID || persona.Version != 2 || persona.Instructions != "Discuss Go tradeoffs." || len(persona.TopicTags) != 1 {
		t.Fatalf("valid persona round trip: %+v", persona)
	}
	broken := map[string]string{
		"unknown":        `{"version":1,"instructions":"x","topic_tags":["a"],"created_at":"2026-09-28T12:00:00Z","extra":1}`,
		"duplicate":      `{"version":1,"instructions":"x","instructions":"y","topic_tags":["a"],"created_at":"2026-09-28T12:00:00Z"}`,
		"case alias":     `{"Version":1,"version":2,"instructions":"x","topic_tags":["a"],"created_at":"2026-09-28T12:00:00Z"}`,
		"escaped alias":  `{"version":1,"\\u0076ersion":2,"instructions":"x","topic_tags":["a"],"created_at":"2026-09-28T12:00:00Z"}`,
		"missing":        `{"version":1,"instructions":"x"}`,
		"trailing":       `{"version":1,"instructions":"x","topic_tags":["a"],"created_at":"2026-09-28T12:00:00Z"} {}`,
		"invalid tags":   `{"version":1,"instructions":"x","topic_tags":["A"],"created_at":"2026-09-28T12:00:00Z"}`,
		"empty content":  `{"version":1,"instructions":"","topic_tags":[],"created_at":"2026-09-28T12:00:00Z"}`,
		"missing time":   `{"version":1,"instructions":"x","topic_tags":["a"]}`,
		"bad time":       `{"version":1,"instructions":"x","topic_tags":["a"],"created_at":"not-a-time"}`,
		"null":           `{"version":1,"instructions":"x","topic_tags":["a"],"created_at":null}`,
		"zero version":   `{"version":0,"instructions":"x","topic_tags":["a"],"created_at":"2026-09-28T12:00:00Z"}`,
		"float version":  `{"version":1.5,"instructions":"x","topic_tags":["a"],"created_at":"2026-09-28T12:00:00Z"}`,
		"quoted version": `{"version":"1","instructions":"x","topic_tags":["a"],"created_at":"2026-09-28T12:00:00Z"}`,
		"overflow":       `{"version":2147483648,"instructions":"x","topic_tags":["a"],"created_at":"2026-09-28T12:00:00Z"}`,
		"invalid utf8":   "{\"version\":1,\"instructions\":\"x\\xffx\",\"topic_tags\":[\"a\"],\"created_at\":\"2026-09-28T12:00:00Z\"}",
		"bad agent":      `{"version":1,"instructions":"x","topic_tags":["a"],"created_at":"2026-09-28T12:00:00Z"}`,
	}
	for name, payload := range broken {
		t.Run(name, func(t *testing.T) {
			agent := agentID
			if name == "bad agent" {
				agent = ""
			}
			if _, decodeErr := DecodeAdminPersona([]byte(payload), agent); decodeErr == nil {
				t.Fatalf("accepted")
			}
		})
	}
	if _, err := DecodeAdminPersona([]byte(`{"version":1,"instructions":"`+strings.Repeat("x", 65000)+`","topic_tags":["a"],"created_at":"2026-09-28T12:00:00Z"}`), agentID); err == nil {
		t.Fatal("oversized persona accepted")
	}
	if _, err := DecodeAdminPersona([]byte(`not json`), agentID); err == nil {
		t.Fatal("non-JSON accepted")
	}
}

func TestDecodeAdminPersonaVersionBoundaries(t *testing.T) {
	agentID := NewID()
	for _, version := range []int{1, 2147483647} {
		payload := fmt.Sprintf(`{"version":%s,"instructions":"x","topic_tags":["a"],"created_at":"2026-09-28T12:00:00Z"}`, strconv.Itoa(version))
		persona, err := DecodeAdminPersona([]byte(payload), agentID)
		if err != nil {
			t.Fatalf("version %d rejected: %v", version, err)
		}
		if persona.Version != version {
			t.Fatalf("version %d got %d", version, persona.Version)
		}
	}
}

func TestDecodeAdminPersonaErrorsDoNotEchoInput(t *testing.T) {
	agentID := NewID()
	secret := "secret-password-do-not-echo"
	control := "\x00\x01\x02"
	cases := map[string]string{
		"unknown secret field": `{"version":1,"instructions":"x","topic_tags":["a"],"created_at":"2026-09-28T12:00:00Z","leak":"` + secret + `"}`,
		"nul in instructions":  `{"version":1,"instructions":"x` + control + `x","topic_tags":["a"],"created_at":"2026-09-28T12:00:00Z"}`,
		"bad time with secret": `{"version":1,"instructions":"x","topic_tags":["a"],"created_at":"` + secret + `"}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			err := decodeErrString(t, func() error {
				_, err := DecodeAdminPersona([]byte(payload), agentID)
				return err
			})
			if err == nil {
				t.Fatal("accepted")
			}
			msg := err.Error()
			if strings.Contains(msg, secret) || strings.Contains(msg, control) {
				t.Fatalf("error echoes input: %q", msg)
			}
		})
	}
}

func decodeErrString(t *testing.T, fn func() error) error {
	t.Helper()
	return fn()
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
	// Validation errors must not echo the offending value.
	if _, err := DecodeAdminPolicy([]byte(`{"version":1,"timezone":"EvilZone","active_start":"09:00","active_end":"17:00",
		"scheduled_min_per_day":1,"scheduled_max_per_day":2,"min_spacing_seconds":3600,"response_min_delay_seconds":60,
		"response_max_delay_seconds":300,"source_max_age_seconds":3600,"reply_probability_bps":2500,"repost_probability_bps":1000,
		"quote_probability_bps":2000,"human_post_probability_bps":500,"continuation_probability_bps":500,"cooldown_seconds":1800,
		"scheduled_post_cap_per_day":2,"reply_cap_per_day":10,"reply_cap_per_conversation":2,"max_agents_per_trigger":2,
		"human_trigger_cap_per_window":5,"human_trigger_window_seconds":3600,"max_chain_depth":2,"max_chain_jobs":5,"daily_token_budget":50000}`)); err == nil {
		t.Fatal("invalid timezone accepted")
	} else if strings.Contains(err.Error(), "EvilZone") {
		t.Fatalf("policy error echoes input: %q", err.Error())
	}
}
