package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func (s *server) trustedOrigin(r *http.Request) bool {
	return len(r.Header.Values("Origin")) == 1 && slices.Contains(s.clientOrigins, r.Header.Get("Origin"))
}

// Set CORS headers before any body/limit/error handling, including preflight.
func (s *server) cors(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		return true
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Add("Vary", "Origin")
	if s.trustedOrigin(r) {
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Expose-Headers", "Location, X-Request-ID, Retry-After")
	}
	if r.Method != http.MethodOptions {
		return true
	}
	w.Header().Add("Vary", "Access-Control-Request-Method")
	w.Header().Add("Vary", "Access-Control-Request-Headers")
	if !s.trustedOrigin(r) || len(r.Header.Values("Access-Control-Request-Method")) != 1 {
		writeError(w, http.StatusForbidden, "forbidden", "Preflight is not allowed")
		return false
	}
	method := r.Header.Get("Access-Control-Request-Method")
	if path.Clean(r.URL.Path) != r.URL.Path {
		writeError(w, http.StatusNotFound, "not_found", "Resource not found")
		return false
	}
	if !slices.Contains([]string{"GET", "HEAD", "POST", "PUT", "DELETE"}, method) {
		writeError(w, http.StatusForbidden, "forbidden", "Preflight is not allowed")
		return false
	}
	for _, line := range r.Header.Values("Access-Control-Request-Headers") {
		for header := range strings.SplitSeq(line, ",") {
			if !slices.Contains([]string{"content-type", "x-csrf-token", "idempotency-key"}, strings.ToLower(strings.TrimSpace(header))) {
				writeError(w, http.StatusForbidden, "forbidden", "Preflight is not allowed")
				return false
			}
		}
	}
	// Ask ServeMux whether the requested method/path actually exists. Fallback
	// patterns have no method prefix and must not gain a successful preflight.
	probe := r.Clone(r.Context())
	probe.Method = method
	_, pattern := s.mux.Handler(probe)
	if !strings.Contains(pattern, " ") {
		writeError(w, http.StatusNotFound, "not_found", "Resource not found")
		return false
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, DELETE")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-CSRF-Token, Idempotency-Key")
	w.WriteHeader(http.StatusNoContent)
	return false
}

func (s *server) cookieName() string {
	if s.secureCookies {
		return "__Host-stacktrace_session"
	}
	return "stacktrace_session_dev"
}

func (s *server) sessionToken(r *http.Request) string {
	cookies := r.CookiesNamed(s.cookieName())
	if len(cookies) != 1 || app.SessionHash(cookies[0].Value) == "" {
		return ""
	}
	return cookies[0].Value
}

func (s *server) setSessionCookie(w http.ResponseWriter, token string) {
	cookie := &http.Cookie{
		Name: s.cookieName(), Value: token, Path: "/", HttpOnly: true,
		Secure: s.secureCookies, SameSite: http.SameSiteLaxMode,
		MaxAge: int(app.SessionLifetime / time.Second), Expires: time.Now().UTC().Add(app.SessionLifetime),
	}
	if token == "" {
		cookie.MaxAge = -1
		cookie.Expires = time.Unix(1, 0).UTC()
	}
	http.SetCookie(w, cookie)
}

func (s *server) csrfToken(token string) string {
	mac := hmac.New(sha256.New, s.csrfSigningKey)
	mac.Write([]byte(token))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
