package app

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPasswordPHCAndExactBytes(t *testing.T) {
	ctx := context.Background()
	password := "  é-secret-password  "
	first, err := hashPassword(ctx, password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := hashPassword(ctx, password)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.HasPrefix(first, passwordPrefix) {
		t.Fatal("missing fresh salt or versioned PHC")
	}
	for _, input := range []string{password, strings.TrimSpace(password), "  e\u0301-secret-password  ", "incorrect-password"} {
		valid, err := verifyPassword(ctx, input, first)
		if err != nil || valid != (input == password) {
			t.Fatalf("exact password comparison: valid=%v error=%v", valid, err)
		}
	}
	for _, hash := range []string{"bad", strings.Replace(first, "m=65536", "m=99999", 1), first + "junk"} {
		if _, err := verifyPassword(ctx, password, hash); !errors.Is(err, ErrUnavailable) {
			t.Fatal("accepted corrupt/unbounded hash")
		}
	}
}

func TestPasswordBoundaries(t *testing.T) {
	for _, tt := range []struct {
		password string
		valid    bool
	}{
		{"", false}, {strings.Repeat("a", 11), false}, {strings.Repeat("a", 12), true},
		{strings.Repeat("a", 1024), true}, {strings.Repeat("a", 1025), false},
		{strings.Repeat("é", 512), true}, {strings.Repeat("é", 513), false},
		{strings.Repeat(" ", 12), true}, {strings.Repeat("a", 12) + "\xff", false},
	} {
		if validPassword(tt.password) != tt.valid {
			t.Errorf("wrong boundary for %d bytes", len(tt.password))
		}
	}
}

type absentCredentialStore struct{ AuthStore }

func (absentCredentialStore) CredentialByHandle(context.Context, string) (Account, string, error) {
	return Account{}, "", ErrNotFound
}

func TestHashCapacityCancellationAndDummyVerification(t *testing.T) {
	for range cap(passwordSlots) {
		passwordSlots <- struct{}{}
	}
	defer func() {
		for range cap(passwordSlots) {
			<-passwordSlots
		}
	}()
	if _, err := hashPassword(context.Background(), "long-enough-password"); !errors.Is(err, ErrBusy) {
		t.Fatalf("hash capacity: %v", err)
	}
	auth := NewAuth(absentCredentialStore{})
	for _, username := range []string{"unknown", "invalid!"} {
		if _, _, err := auth.Login(context.Background(), username, "long-enough-password", ""); !errors.Is(err, ErrBusy) {
			t.Fatalf("unknown login skipped dummy verification: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hashPassword(ctx, "long-enough-password"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestSessionCredentials(t *testing.T) {
	first, token := newSession(NewID())
	second, other := newSession(first.AccountID)
	if token == other || first.TokenHash == second.TokenHash || len(first.TokenHash) != 64 || first.TokenHash == token || SessionHash(token) != first.TokenHash || first.ExpiresAt.Sub(first.CreatedAt) != SessionLifetime {
		t.Fatal("invalid session credential/lifetime")
	}
	for _, bad := range []string{"", "short", strings.Repeat("!", 43), token + "="} {
		if SessionHash(bad) != "" {
			t.Fatal("accepted malformed credential")
		}
	}
}

func BenchmarkPasswordHash(b *testing.B) {
	for b.Loop() {
		if _, err := hashPassword(context.Background(), "benchmark-password"); err != nil {
			b.Fatal(err)
		}
	}
}
