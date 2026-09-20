package postgres

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

var errSeedFixture = errors.New("invalid seed fixture")

type demoFixture struct {
	accounts []app.Account
	follows  [][2]app.ID
	personas []app.Persona
	settings []app.AgentSettings
}

// These operator-owned fixtures have explicit required fields, not defaults.
// Validate both completely before starting the seed transaction.
func decodeDemoFixture(demo, personas []byte) (demoFixture, error) {
	var fixture demoFixture
	var identities struct {
		Accounts []json.RawMessage `json:"accounts"`
		Follows  []json.RawMessage `json:"follows"`
	}
	if decodeFixtureObject(demo, &identities, "accounts", "follows") != nil || len(identities.Accounts) == 0 {
		return fixture, errSeedFixture
	}
	ids := make(map[app.ID]bool)
	handles := make(map[string]app.ID)
	for _, data := range identities.Accounts {
		var row struct {
			ID          string `json:"id"`
			Handle      string `json:"handle"`
			DisplayName string `json:"display_name"`
			Initials    string `json:"initials"`
			Bio         string `json:"bio"`
			RoleLabel   string `json:"role_label"`
			Specialty   string `json:"specialty"`
		}
		if decodeFixtureObject(data, &row, "id", "handle", "display_name", "initials", "bio", "role_label", "specialty") != nil {
			return fixture, errSeedFixture
		}
		account, err := app.NewAccount(app.AccountAgent, row.Handle, row.DisplayName, time.Now())
		if err != nil || account.Handle != row.Handle || handles[row.Handle] != "" {
			return fixture, errSeedFixture
		}
		account.ID, err = app.ParseID(row.ID)
		if err != nil || string(account.ID) != row.ID || ids[account.ID] || account.ID == "00000000-0000-0000-0000-000000000000" {
			return fixture, errSeedFixture
		}
		account.Initials, account.Bio, account.RoleLabel, account.Specialty = row.Initials, row.Bio, row.RoleLabel, row.Specialty
		for _, text := range []string{row.Initials, row.Bio, row.RoleLabel, row.Specialty} {
			if strings.ContainsRune(text, 0) {
				return fixture, errSeedFixture
			}
		}
		ids[account.ID], handles[account.Handle] = true, account.ID
		fixture.accounts = append(fixture.accounts, account)
	}
	seenFollows := make(map[[2]app.ID]bool)
	for _, data := range identities.Follows {
		var row struct {
			Follower string `json:"follower"`
			Followed string `json:"followed"`
		}
		if decodeFixtureObject(data, &row, "follower", "followed") != nil {
			return fixture, errSeedFixture
		}
		pair := [2]app.ID{handles[row.Follower], handles[row.Followed]}
		if pair[0] == "" || pair[1] == "" || pair[0] == pair[1] || seenFollows[pair] {
			return fixture, errSeedFixture
		}
		seenFollows[pair] = true
		fixture.follows = append(fixture.follows, pair)
	}
	var versions struct {
		Personas []json.RawMessage `json:"personas"`
	}
	if decodeFixtureObject(personas, &versions, "personas") != nil || len(versions.Personas) != len(fixture.accounts) {
		return fixture, errSeedFixture
	}
	seenPersonas := make(map[app.ID]bool)
	for _, data := range versions.Personas {
		var row struct {
			AgentID      app.ID          `json:"agent_id"`
			Version      int             `json:"version"`
			Instructions string          `json:"instructions"`
			TopicTags    []string        `json:"topic_tags"`
			CreatedAt    time.Time       `json:"created_at"`
			Enabled      bool            `json:"enabled"`
			Policy       json.RawMessage `json:"policy"`
		}
		if decodeFixtureObject(data, &row, "agent_id", "version", "instructions", "topic_tags", "created_at", "enabled", "policy") != nil || !ids[row.AgentID] || seenPersonas[row.AgentID] || row.Enabled {
			return fixture, errSeedFixture
		}
		persona := app.Persona{AgentID: row.AgentID, Version: row.Version, Instructions: row.Instructions, TopicTags: row.TopicTags, CreatedAt: row.CreatedAt}
		policy, err := app.DecodeGenerationPolicy(row.Policy)
		settings := app.AgentSettings{AgentID: row.AgentID, PersonaVersion: row.Version, Policy: policy, UpdatedAt: row.CreatedAt}
		if err != nil || persona.Validate() != nil || settings.Validate() != nil || persona.CreatedAt.Nanosecond()%1000 != 0 {
			return fixture, errSeedFixture
		}
		seenPersonas[row.AgentID] = true
		fixture.personas = append(fixture.personas, persona)
		fixture.settings = append(fixture.settings, settings)
	}
	return fixture, nil
}

// Only the small fixture objects use this check. Policy decoding remains owned
// by app. In particular encoding/json alone accepts duplicate and case-alias keys.
func decodeFixtureObject(data []byte, target any, keys ...string) error {
	if len(data) > 128*1024 || !utf8.Valid(data) {
		return errSeedFixture
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return errSeedFixture
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || !slices.Contains(keys, name) || seen[name] {
			return errSeedFixture
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errSeedFixture
		}
		seen[name] = true
	}
	if _, err := decoder.Token(); err != nil || len(seen) != len(keys) {
		return errSeedFixture
	}
	if decoder.Decode(new(any)) != io.EOF || json.Unmarshal(data, target) != nil {
		return errSeedFixture
	}
	return nil
}
