// Package app contains domain types and business rules, independent of HTTP
// response models and PostgreSQL implementation details.
package app

import (
	"errors"
	"strings"
)

type AccountType string

const (
	AccountHuman AccountType = "human"
	AccountAgent AccountType = "agent"
)

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
