// Package app contains domain types and business rules, independent of HTTP
// response models and PostgreSQL implementation details.
package app

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

type AccountType string

const (
	AccountHuman AccountType = "human"
	AccountAgent AccountType = "agent"
)

type Account struct {
	ID            ID
	Kind          AccountType
	Handle        string
	DisplayName   string
	Initials      string
	Bio           string
	RoleLabel     string
	StatusText    string
	Specialty     string
	AppearanceKey *string
	VerifiedAt    *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DisabledAt    *time.Time
}

// NewAccount establishes identity and UTC timestamps for trusted application
// callers. Registration will always select AccountHuman itself.
func NewAccount(kind AccountType, handle, displayName string, now time.Time) (Account, error) {
	fields := make(map[string]string)
	if kind != AccountHuman && kind != AccountAgent {
		fields["kind"] = "Must be human or agent"
	}
	normalizedHandle, err := NormalizeHandle(handle)
	if err != nil {
		fields["handle"] = ErrInvalidHandle.Error()
	}
	displayName = strings.TrimSpace(displayName)
	displayNameLength := utf8.RuneCountInString(displayName)
	if !utf8.ValidString(displayName) || displayNameLength < 1 || displayNameLength > 80 || strings.ContainsRune(displayName, 0) {
		fields["display_name"] = "Must contain 1-80 Unicode code points"
	}
	if len(fields) != 0 {
		return Account{}, &ValidationError{Fields: fields}
	}
	return Account{
		ID:          NewID(),
		Kind:        kind,
		Handle:      normalizedHandle,
		DisplayName: displayName,
		CreatedAt:   now.UTC(),
		UpdatedAt:   now.UTC(),
	}, nil
}

var ErrInvalidHandle = errors.New("handle must contain 3-32 ASCII letters, digits, or underscores")

// NormalizeHandle normalizes public handles and human login usernames.
// Handles are stored lowercase without an @ prefix. Passwords must never pass
// through this normalization.
func NormalizeHandle(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 3 || len(value) > 32 {
		return "", ErrInvalidHandle
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return "", ErrInvalidHandle
		}
	}
	return strings.ToLower(value), nil
}
