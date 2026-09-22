package app

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGenerationMentions(t *testing.T) {
	for _, tt := range []struct {
		body string
		want []string
	}{
		{"@Go_Agent (@SQL123), @go_agent! @unknown.", []string{"go_agent", "sql123", "unknown"}},
		{"foo@golang.com x@golang @golang.dev @@golang foo_@golang foo-@golang /@golang @golang/suffix @golang-dev", nil},
		{"é@golang @golangé @golang\u0301 @ab @" + strings.Repeat("a", 33), nil},
		{"hello: @golang; [@postgres] <@other_agent>", []string{"golang", "postgres", "other_agent"}},
		{"@" + strings.Repeat("A", 32), []string{strings.Repeat("a", 32)}},
		{strings.Repeat("x", MaxBodyRunes*4) + " @golang", nil},
		{"@golang\xff", nil},
	} {
		if got := GenerationMentions(tt.body); !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("%q: %v, want %v", tt.body, got, tt.want)
		}
	}
	var body strings.Builder
	for i := 0; i < 40; i++ {
		body.WriteString(" @agent_")
		body.WriteByte(byte('A' + i/10))
		body.WriteByte(byte('0' + i%10))
	}
	if got := GenerationMentions(body.String()); len(got) != MaxGenerationMentions || got[0] != "agent_a0" || got[31] != "agent_d1" {
		t.Fatalf("bounded ordered mentions: %v", got)
	}
}

func TestGenerationStableKeys(t *testing.T) {
	at := scheduleTime("2026-09-20T10:00:00.123456789Z")
	key, err := ScheduledGenerationKey(at)
	same, _ := ScheduledGenerationKey(at.In(time.FixedZone("elsewhere", 3600)).Truncate(time.Microsecond))
	other, _ := ScheduledGenerationKey(at.Add(time.Microsecond))
	if err != nil || key != same || key == other {
		t.Fatalf("slot keys: %q %q %q %v", key, same, other, err)
	}
	actor, agent, conversation, action := NewID(), NewID(), NewID(), NewID()
	trigger, err := SocialGenerationKey(TriggerRepost, action)
	duplicate, _ := SocialGenerationKey(TriggerRepost, action)
	recreated, _ := SocialGenerationKey(TriggerRepost, NewID())
	if err != nil || trigger != duplicate || trigger == recreated {
		t.Fatal("unstable action identity")
	}
	cooldown, err := GenerationCooldownKey(actor, agent, conversation, TriggerRepost)
	again, _ := GenerationCooldownKey(actor, agent, conversation, TriggerRepost)
	if err != nil || cooldown != again || len(cooldown) > 256 {
		t.Fatal("unstable cooldown")
	}
	for _, change := range []struct {
		actor, agent, conversation ID
		kind                       GenerationTrigger
	}{
		{NewID(), agent, conversation, TriggerRepost}, {actor, NewID(), conversation, TriggerRepost}, {actor, agent, NewID(), TriggerRepost}, {actor, agent, conversation, TriggerReply},
	} {
		key, err := GenerationCooldownKey(change.actor, change.agent, change.conversation, change.kind)
		if err != nil || key == cooldown {
			t.Fatal("cooldown lost identity dimension")
		}
	}
	if _, err := SocialGenerationKey(TriggerScheduled, action); err == nil {
		t.Fatal("accepted scheduled social key")
	}
	if _, err := SocialGenerationKey(TriggerReply, "bad"); err == nil {
		t.Fatal("accepted invalid action")
	}
	if _, err := GenerationCooldownKey(actor, actor, conversation, TriggerReply); err == nil {
		t.Fatal("accepted self trigger")
	}
	if _, err := ScheduledGenerationKey(time.Time{}); err == nil {
		t.Fatal("accepted zero slot")
	}
}

func TestGenerationResponseTiming(t *testing.T) {
	p := testGenerationPolicy()
	p.ResponseMinDelaySeconds, p.ResponseMaxDelaySeconds, p.SourceMaxAgeSeconds = 10, 20, 60
	source := scheduleTime("2026-09-20T02:00:00.123456789Z") // Outside active hours.
	available, expires, err := GenerationResponseTiming(p, source, source, func(n int64) int64 { return n - 1 })
	if err != nil || !available.Equal(GenerationInstant(source).Add(20*time.Second)) || !expires.Equal(GenerationInstant(source).Add(time.Minute)) {
		t.Fatal(available, expires, err)
	}
	now := source.Add(30 * time.Second)
	available, sameExpiry, err := GenerationResponseTiming(p, source, now, func(int64) int64 { return 0 })
	if err != nil || !available.Equal(GenerationInstant(now)) || !sameExpiry.Equal(expires) {
		t.Fatal("late source expiry changed", available, sameExpiry, err)
	}
	if _, _, err := GenerationResponseTiming(p, source, expires, nil); err == nil {
		t.Fatal("revived expired source")
	}
	if _, _, err := GenerationResponseTiming(p, source, source.Add(-time.Second), nil); err == nil {
		t.Fatal("accepted future source")
	}
}
