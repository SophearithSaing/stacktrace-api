package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"
)

const SessionLifetime = 7 * 24 * time.Hour

type Session struct {
	TokenHash string
	AccountID ID
	CreatedAt time.Time
	ExpiresAt time.Time
}

// AuthStore operations own their atomic persistence boundaries. Registration
// commits the human, credential and rotated session together.
type AuthStore interface {
	RegisterHuman(context.Context, Account, string, Session, string) error
	CredentialByHandle(context.Context, string) (Account, string, error)
	RotateSession(context.Context, Session, string) error
	SessionAccount(context.Context, string) (Account, error)
	RevokeSession(context.Context, string) error
}

type Auth struct{ store AuthStore }

func NewAuth(store AuthStore) *Auth { return &Auth{store: store} }

func (a *Auth) Register(ctx context.Context, username, password, displayName, previousToken string) (Account, string, error) {
	account, err := NewAccount(AccountHuman, username, displayName, time.Now())
	if err != nil {
		var validation *ValidationError
		if errors.As(err, &validation) && validation.Fields["handle"] != "" {
			validation.Fields["username"] = validation.Fields["handle"]
			delete(validation.Fields, "handle")
		}
		return Account{}, "", err
	}
	passwordHash, err := hashPassword(ctx, password)
	if err != nil {
		return Account{}, "", err
	}
	session, token := newSession(account.ID)
	if err := a.store.RegisterHuman(ctx, account, passwordHash, session, SessionHash(previousToken)); err != nil {
		return Account{}, "", err
	}
	return account, token, nil
}

func (a *Auth) Login(ctx context.Context, username, password, previousToken string) (Account, string, error) {
	if !validPassword(password) {
		return Account{}, "", ErrInvalidCredentials
	}
	handle, handleErr := NormalizeHandle(username)
	var account Account
	passwordHash := dummyPasswordHash
	found := false
	if handleErr == nil {
		var err error
		account, passwordHash, err = a.store.CredentialByHandle(ctx, handle)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return Account{}, "", err
		}
		found = err == nil
		if !found {
			passwordHash = dummyPasswordHash
		}
	}
	valid, err := verifyPassword(ctx, password, passwordHash)
	if err != nil {
		return Account{}, "", err
	}
	if !found || !valid || account.Type != AccountHuman || account.DisabledAt != nil {
		return Account{}, "", ErrInvalidCredentials
	}
	session, token := newSession(account.ID)
	if err := a.store.RotateSession(ctx, session, SessionHash(previousToken)); err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			err = ErrInvalidCredentials
		}
		return Account{}, "", err
	}
	return account, token, nil
}

func newSession(accountID ID) (Session, string) {
	raw := make([]byte, 32)
	rand.Read(raw)
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now().UTC()
	return Session{SessionHash(token), accountID, now, now.Add(SessionLifetime)}, token
}

// SessionHash accepts only canonical 256-bit credentials. Empty means absent.
func SessionHash(token string) string {
	if len(token) != 43 {
		return ""
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) != 32 {
		return ""
	}
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}
