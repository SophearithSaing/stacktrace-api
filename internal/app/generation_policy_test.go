package app

import (
	"encoding/json"
	"strings"
	"testing"
)

func testGenerationPolicy() GenerationPolicy {
	return GenerationPolicy{
		Version: 1, Timezone: "UTC", ActiveStart: "09:00", ActiveEnd: "17:00",
		ScheduledMinPerDay: 1, ScheduledMaxPerDay: 2, MinSpacingSeconds: 3600,
		ResponseMinDelaySeconds: 60, ResponseMaxDelaySeconds: 300, SourceMaxAgeSeconds: 3600,
		ReplyProbabilityBPS: 2500, RepostProbabilityBPS: 1000, QuoteProbabilityBPS: 2000,
		HumanPostProbabilityBPS: 500, ContinuationProbabilityBPS: 500, CooldownSeconds: 1800,
		ScheduledPostCapPerDay: 2, ReplyCapPerDay: 10, ReplyCapPerConversation: 2,
		MaxAgentsPerTrigger: 2, HumanTriggerCapPerWindow: 5, HumanTriggerWindowSeconds: 3600,
		MaxChainDepth: 2, MaxChainJobs: 5, DailyTokenBudget: 50000,
	}
}

func TestGenerationPolicy(t *testing.T) {
	valid := testGenerationPolicy()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*GenerationPolicy){
		"version":                  func(p *GenerationPolicy) { p.Version = 2 },
		"timezone":                 func(p *GenerationPolicy) { p.Timezone = "Mars/Olympus" },
		"host timezone":            func(p *GenerationPolicy) { p.Timezone = "Local" },
		"empty timezone":           func(p *GenerationPolicy) { p.Timezone = "" },
		"overnight":                func(p *GenerationPolicy) { p.ActiveEnd = "08:00" },
		"empty window":             func(p *GenerationPolicy) { p.ActiveEnd = p.ActiveStart },
		"hour format":              func(p *GenerationPolicy) { p.ActiveStart = "9:00" },
		"invalid minute":           func(p *GenerationPolicy) { p.ActiveEnd = "17:60" },
		"reversed schedule":        func(p *GenerationPolicy) { p.ScheduledMinPerDay = 3 },
		"schedule cap":             func(p *GenerationPolicy) { p.ScheduledPostCapPerDay = 1 },
		"infeasible spacing":       func(p *GenerationPolicy) { p.MinSpacingSeconds = 8 * 3600 },
		"zero spacing":             func(p *GenerationPolicy) { p.MinSpacingSeconds = 0 },
		"reversed delays":          func(p *GenerationPolicy) { p.ResponseMinDelaySeconds = 301 },
		"stale delay":              func(p *GenerationPolicy) { p.SourceMaxAgeSeconds = 300 },
		"negative probability":     func(p *GenerationPolicy) { p.ReplyProbabilityBPS = -1 },
		"large probability":        func(p *GenerationPolicy) { p.RepostProbabilityBPS = 10001 },
		"quote probability":        func(p *GenerationPolicy) { p.QuoteProbabilityBPS = 10001 },
		"human probability":        func(p *GenerationPolicy) { p.HumanPostProbabilityBPS = 10001 },
		"continuation probability": func(p *GenerationPolicy) { p.ContinuationProbabilityBPS = 10001 },
		"cooldown":                 func(p *GenerationPolicy) { p.CooldownSeconds = 0 },
		"unbounded replies":        func(p *GenerationPolicy) { p.ReplyCapPerDay = 1001 },
		"conversation cap":         func(p *GenerationPolicy) { p.ReplyCapPerConversation = -1 },
		"selection":                func(p *GenerationPolicy) { p.MaxAgentsPerTrigger = 11 },
		"human cap":                func(p *GenerationPolicy) { p.HumanTriggerCapPerWindow = -1 },
		"human window":             func(p *GenerationPolicy) { p.HumanTriggerWindowSeconds = 0 },
		"chain depth":              func(p *GenerationPolicy) { p.MaxChainDepth = 11 },
		"chain total":              func(p *GenerationPolicy) { p.MaxChainJobs = 2 },
		"budget":                   func(p *GenerationPolicy) { p.DailyTokenBudget = 10000001 },
	} {
		t.Run(name, func(t *testing.T) {
			p := valid
			change(&p)
			if err := p.Validate(); err == nil {
				t.Fatal("accepted invalid policy")
			}
		})
	}
	valid.ScheduledMinPerDay, valid.ScheduledMaxPerDay, valid.ScheduledPostCapPerDay = 0, 0, 0
	valid.ReplyCapPerDay, valid.ReplyCapPerConversation, valid.MaxAgentsPerTrigger = 0, 0, 0
	valid.HumanTriggerCapPerWindow, valid.MaxChainDepth, valid.MaxChainJobs, valid.DailyTokenBudget = 0, 0, 1, 0
	valid.ReplyProbabilityBPS, valid.RepostProbabilityBPS = 0, 10000
	if err := valid.Validate(); err != nil {
		t.Fatalf("explicit disable/boundary: %v", err)
	}
}

func TestDecodeGenerationPolicy(t *testing.T) {
	p := testGenerationPolicy()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeGenerationPolicy(data)
	if err != nil || got != p {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	for name, data := range map[string]string{
		"null": "null", "array": "[]", "empty": "{}", "truncated": "{",
		"trailing": string(data) + " {}", "trailing garbage": string(data) + " x",
		"unknown":             strings.Replace(string(data), "{", `{"extra":1,`, 1),
		"duplicate":           strings.Replace(string(data), "{", `{"version":1,`, 1),
		"wrong type":          strings.Replace(string(data), `"version":1`, `"version":"1"`, 1),
		"fraction":            strings.Replace(string(data), `"version":1`, `"version":1.5`, 1),
		"overflow":            strings.Replace(string(data), `"version":1`, `"version":999999999999999999999999`, 1),
		"case alias":          strings.Replace(string(data), "{", `{"VERSION":1,`, 1),
		"unsupported version": strings.Replace(string(data), `"version":1`, `"version":2`, 1),
		"oversized":           strings.Repeat(" ", 8193) + string(data),
		"nested value":        strings.Replace(string(data), `"version":1`, `"version":{}`, 1),
		"nonfinite number":    strings.Replace(string(data), `"version":1`, `"version":NaN`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeGenerationPolicy([]byte(data)); err == nil {
				t.Fatal("accepted invalid JSON policy")
			}
		})
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for name, value := range fields {
		delete(fields, name)
		missing, _ := json.Marshal(fields)
		if _, err := DecodeGenerationPolicy(missing); err == nil {
			t.Errorf("accepted missing %s", name)
		}
		fields[name] = json.RawMessage("null")
		null, _ := json.Marshal(fields)
		if _, err := DecodeGenerationPolicy(null); err == nil {
			t.Errorf("accepted null %s", name)
		}
		fields[name] = value
	}
}
