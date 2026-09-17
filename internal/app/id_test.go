package app

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestID(t *testing.T) {
	first, second := NewID(), NewID()
	if first == second || len(first) != 36 || first[14] != '4' || !strings.ContainsRune("89ab", rune(first[19])) {
		t.Fatal("invalid random UUID v4")
	}
	parsed, err := ParseID(strings.ToUpper(string(first)))
	if err != nil || parsed != first {
		t.Fatalf("UUID round trip failed: %v", err)
	}
	for _, invalid := range []string{"", "123", strings.Repeat("a", 36), string(first) + " ", "z" + string(first)[1:]} {
		if _, err := ParseID(invalid); !errors.Is(err, ErrInvalidID) {
			t.Errorf("accepted invalid UUID %q", invalid)
		}
	}
}

func TestNewAccount(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.FixedZone("local", 7*3600))
	account, err := NewAccount(AccountAgent, " Go_Lang ", " Go ", now)
	if err != nil || account.Type != AccountAgent || account.Handle != "go_lang" || account.DisplayName != "Go" || !account.CreatedAt.Equal(now) || account.CreatedAt.Location() != time.UTC || account.UpdatedAt != account.CreatedAt {
		t.Fatalf("invalid normalized account: %+v %v", account, err)
	}
	_, err = NewAccount("robot", "@go", " ", now)
	var validation *ValidationError
	if !errors.As(err, &validation) || len(validation.Fields) != 3 || validation.Fields["type"] != "Must be human or agent" {
		t.Fatalf("missing domain validation: %v", err)
	}
	if _, err := NewAccount(AccountHuman, "human", strings.Repeat("🙂", 80), now); err != nil {
		t.Fatal("display name limit should count code points")
	}
	if _, err := NewAccount(AccountHuman, "human", strings.Repeat("🙂", 81), now); err == nil {
		t.Fatal("accepted oversized display name")
	}
}
