package app

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Passwords are 12-1024 UTF-8 bytes, used exactly as submitted. These fixed
// parameters bound verification too; new parameter versions need an explicit
// verifier before any stored hashes are upgraded.
const passwordPrefix = "$argon2id$v=19$m=65536,t=3,p=2$"

// At most 128 MiB of Argon2 working memory per process; do not queue bursts.
var passwordSlots = make(chan struct{}, 2)

// A valid PHC record used for the same expensive verification on absent accounts.
const dummyPasswordHash = passwordPrefix + "AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func validPassword(password string) bool {
	return len(password) >= 12 && len(password) <= 1024 && utf8.ValidString(password)
}

func passwordKey(ctx context.Context, password string, salt []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case passwordSlots <- struct{}{}:
		defer func() { <-passwordSlots }()
	default:
		return nil, ErrBusy
	}
	key := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return key, nil
}

func hashPassword(ctx context.Context, password string) (string, error) {
	if !validPassword(password) {
		return "", &ValidationError{Fields: map[string]string{"password": "Must contain 12-1024 UTF-8 bytes"}}
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := passwordKey(ctx, password, salt)
	if err != nil {
		return "", err
	}
	return passwordPrefix + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}

func verifyPassword(ctx context.Context, password, encoded string) (bool, error) {
	if !validPassword(password) {
		return false, nil
	}
	if len(encoded) != len(dummyPasswordHash) || !strings.HasPrefix(encoded, passwordPrefix) {
		return false, ErrUnavailable
	}
	parts := strings.Split(strings.TrimPrefix(encoded, passwordPrefix), "$")
	if len(parts) != 2 {
		return false, ErrUnavailable
	}
	salt, saltErr := base64.RawStdEncoding.Strict().DecodeString(parts[0])
	expected, keyErr := base64.RawStdEncoding.Strict().DecodeString(parts[1])
	if saltErr != nil || keyErr != nil || len(salt) != 16 || len(expected) != 32 {
		return false, ErrUnavailable
	}
	actual, err := passwordKey(ctx, password, salt)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}
