package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

type meResponse struct {
	Account   *accountSummary `json:"account"`
	CSRFToken *string         `json:"csrf_token"`
}

// register handles account registration requests.
func (s *server) register(w http.ResponseWriter, r *http.Request) {
	if !s.credentialAttempt(w, r) {
		return
	}
	var input struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeFailure(w, err)
		return
	}
	if !s.allowUsername(w, input.Username) {
		return
	}
	account, token, err := s.auth.Register(r.Context(), input.Username, input.Password, input.DisplayName, s.sessionToken(r))
	if err != nil {
		writeFailure(w, err)
		return
	}
	s.setSessionCookie(w, token)
	w.Header().Set("Location", "/api/v1/accounts/"+string(account.ID))
	s.writeMe(w, http.StatusCreated, account, token)
}

// login handles login requests.
func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if !s.credentialAttempt(w, r) {
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeFailure(w, err)
		return
	}
	if !s.allowUsername(w, input.Username) {
		return
	}
	account, token, err := s.auth.Login(r.Context(), input.Username, input.Password, s.sessionToken(r))
	if err != nil {
		writeFailure(w, err)
		return
	}
	s.setSessionCookie(w, token)
	s.writeMe(w, http.StatusOK, account, token)
}

// writeMe writes an authenticated account response and session cookie.
func (s *server) writeMe(w http.ResponseWriter, status int, account app.Account, token string) {
	summary, csrf := summarizeAccount(account), s.csrfToken(token)
	writeJSON(w, status, meResponse{Account: &summary, CSRFToken: &csrf})
}

// me handles current-account requests.
func (s *server) me(w http.ResponseWriter, r *http.Request) {
	account, err := s.viewer(r)
	if err != nil {
		writeFailure(w, err)
		return
	}
	if account.ID == "" {
		writeJSON(w, http.StatusOK, meResponse{})
		return
	}
	s.writeMe(w, http.StatusOK, account, s.sessionToken(r))
}

// logout handles session revocation requests.
func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWrite(w, r); !ok {
		return
	}
	if err := s.store.RevokeSession(r.Context(), app.SessionHash(s.sessionToken(r))); err != nil {
		writeFailure(w, err)
		return
	}
	s.setSessionCookie(w, "")
	w.WriteHeader(http.StatusNoContent)
}

// viewer returns the authenticated viewer, treating stale sessions as anonymous.
// A stale/expired/disabled session is anonymous on public reads; outages are not.
func (s *server) viewer(r *http.Request) (app.Account, error) {
	token := s.sessionToken(r)
	if token == "" {
		return app.Account{}, nil
	}
	account, err := s.store.SessionAccount(r.Context(), app.SessionHash(token))
	if errors.Is(err, app.ErrUnauthenticated) {
		return app.Account{}, nil
	}
	return account, err
}

// requireWrite authenticates and authorizes a state-changing request.
func (s *server) requireWrite(w http.ResponseWriter, r *http.Request) (app.Account, bool) {
	if !s.trustedOrigin(r) {
		writeFailure(w, app.ErrForbidden)
		return app.Account{}, false
	}
	account, err := s.viewer(r)
	if err != nil {
		writeFailure(w, err)
		return app.Account{}, false
	}
	if account.ID == "" {
		writeFailure(w, app.ErrUnauthenticated)
		return app.Account{}, false
	}
	if len(r.Header.Values("X-CSRF-Token")) != 1 || !hmac.Equal([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.csrfToken(s.sessionToken(r)))) {
		writeFailure(w, app.ErrForbidden)
		return app.Account{}, false
	}
	if !allowRate(w, s.accountLimiter, string(account.ID)) {
		return app.Account{}, false
	}
	return account, true
}

// credentialAttempt applies the connection-scoped credential rate limit.
func (s *server) credentialAttempt(w http.ResponseWriter, r *http.Request) bool {
	if !s.trustedOrigin(r) {
		writeFailure(w, app.ErrForbidden)
		return false
	}
	peer, _ := netip.ParseAddrPort(r.RemoteAddr)
	return allowRate(w, s.credentialIPLimiter, peer.Addr().Unmap().String())
}

// allowUsername applies the username-scoped credential rate limit.
func (s *server) allowUsername(w http.ResponseWriter, username string) bool {
	handle, _ := app.NormalizeHandle(username)
	// Keep bounded, normalized keys without retaining submitted usernames.
	hash := sha256.Sum256([]byte(handle))
	return allowRate(w, s.usernameLimiter, hex.EncodeToString(hash[:]))
}

// allowRate applies a rate limit and writes a rejection when the key is limited.
func allowRate(w http.ResponseWriter, limiter *rateLimiter, key string) bool {
	if retry := limiter.allow(key, time.Now()); retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int((retry+time.Second-1)/time.Second))))
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Request rate limit exceeded")
		return false
	}
	return true
}
