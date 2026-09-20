package postgres

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/seed"
)

func TestSeedFixtures(t *testing.T) {
	fixture, err := decodeDemoFixture(seed.Demo, seed.Personas)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixture.accounts) != 2 || len(fixture.follows) != 2 || len(fixture.personas) != 2 || len(fixture.settings) != 2 {
		t.Fatal("incomplete demo")
	}
	for i, persona := range fixture.personas {
		settings := fixture.settings[i]
		if persona.AgentID != fixture.accounts[i].ID || persona.Version != 1 || settings.Enabled || settings.Policy != generationPolicy() {
			t.Fatal("unexpected initial configuration")
		}
		if !strings.Contains(persona.Instructions, "unofficial AI") || !strings.Contains(persona.Instructions, "untrusted") {
			t.Fatal("missing persona boundaries")
		}
	}
}

func TestSeedFixturesRejectInvalidData(t *testing.T) {
	for _, tc := range []struct{ name, old, replacement string }{
		{"unknown", `"version": 1,`, `"unknown": "secret", "version": 1,`},
		{"duplicate", `"version": 1,`, `"version": 1, "version": 1,`},
		{"case alias", `"version": 1,`, `"Version": 1,`},
		{"missing", `"enabled": false,`, ``},
		{"null", `"enabled": false`, `"enabled": null`},
		{"enabled", `"enabled": false`, `"enabled": true`},
		{"foreign agent", `aacebb3d-a794-4cf6-ae22-c8e0a7ca1a01`, `aacebb3d-a794-4cf6-ae22-c8e0a7ca1aff`},
		{"duplicate agent", `aacebb3d-a794-4cf6-ae22-c8e0a7ca1a02`, `aacebb3d-a794-4cf6-ae22-c8e0a7ca1a01`},
		{"topic", `"go",`, `"not a slug",`},
		{"empty instructions", `"instructions": "You are Go`, `"instructions": "\u0000You are Go`},
		{"timestamp", `2026-09-20T00:00:00Z`, `not-a-time`},
		{"timestamp precision", `2026-09-20T00:00:00Z`, `2026-09-20T00:00:00.000000001Z`},
		{"policy unknown", `"daily_token_budget": 50000`, `"secret": "sensitive", "daily_token_budget": 50000`},
		{"policy missing", `"daily_token_budget": 50000`, `"different": 50000`},
		{"policy duplicate", `"daily_token_budget": 50000`, `"daily_token_budget": 50000, "daily_token_budget": 1`},
		{"policy null", `"daily_token_budget": 50000`, `"daily_token_budget": null`},
		{"policy invalid", `"daily_token_budget": 50000`, `"daily_token_budget": -1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := bytes.Replace(seed.Personas, []byte(tc.old), []byte(tc.replacement), 1)
			if bytes.Equal(data, seed.Personas) {
				t.Fatal("test did not change fixture")
			}
			if _, err := decodeDemoFixture(seed.Demo, data); !errors.Is(err, errSeedFixture) {
				t.Fatalf("got %v", err)
			}
		})
	}
	for _, data := range [][]byte{[]byte(`null`), []byte(`{"personas":[]}`), append(bytes.Clone(seed.Personas), []byte(` {}`)...)} {
		if _, err := decodeDemoFixture(seed.Demo, data); !errors.Is(err, errSeedFixture) {
			t.Fatalf("got %v", err)
		}
	}
	for _, tc := range []struct{ old, replacement string }{
		{`"accounts":`, `"Accounts":`},
		{`"handle": "golang"`, `"handle": "bad handle"`},
		{`"bio":`, `"unknown":`},
		{`"follower": "golang"`, `"follower": "absent"`},
		{`"followed": "postgresql"`, `"followed": "golang"`},
		{`"bio": "An AI`, `"bio": "\u0000An AI`},
	} {
		data := bytes.Replace(seed.Demo, []byte(tc.old), []byte(tc.replacement), 1)
		if _, err := decodeDemoFixture(data, seed.Personas); !errors.Is(err, errSeedFixture) {
			t.Fatalf("invalid demo accepted: %v", err)
		}
	}
}
