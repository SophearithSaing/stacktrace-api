package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Any unexpected store access panics, so preflight/Origin tests prove that
// rejection and preflight happen before authentication and credential work.
type unusedStore struct{ Store }

func (unusedStore) Ready(context.Context) error { return nil }

func TestCORSAndOriginGuards(t *testing.T) {
	handler := NewHandler(unusedStore{}, []string{"http://localhost:5173"}, []byte(strings.Repeat("k", 32)), false, testLogger())
	for _, tt := range []struct {
		name, method, path, origin, requestedMethod, headers string
		status                                               int
	}{
		{"allowed preflight", "OPTIONS", "/api/v1/auth/logout", "http://localhost:5173", "POST", "Content-Type, X-CSRF-Token, Idempotency-Key", 204},
		{"null", "OPTIONS", "/api/v1/auth/logout", "null", "POST", "", 403},
		{"missing", "OPTIONS", "/api/v1/auth/logout", "", "POST", "", 403},
		{"suffix", "OPTIONS", "/api/v1/auth/logout", "http://localhost:5173.evil.test", "POST", "", 403},
		{"method", "OPTIONS", "/api/v1/auth/logout", "http://localhost:5173", "PATCH", "", 403},
		{"header", "OPTIONS", "/api/v1/auth/logout", "http://localhost:5173", "POST", "Authorization", 403},
		{"unknown path", "OPTIONS", "/api/v1/nope", "http://localhost:5173", "POST", "", 404},
		{"wrong path method", "OPTIONS", "/api/v1/auth/logout", "http://localhost:5173", "GET", "", 404},
		{"noncanonical path", "OPTIONS", "/api/v1/auth/../auth/logout", "http://localhost:5173", "POST", "", 404},
		{"register no origin", "POST", "/api/v1/auth/register", "", "", "", 403},
		{"login null", "POST", "/api/v1/auth/login", "null", "", "", 403},
		{"logout denied", "POST", "/api/v1/auth/logout", "https://evil.test", "", "", 403},
		{"api error", "GET", "/api/v1/nope", "http://localhost:5173", "", "", 404},
		{"method error", "DELETE", "/api/v1/auth/login", "http://localhost:5173", "", "", 405},
		{"profile method error", "POST", "/api/v1/accounts/by-handle/test", "http://localhost:5173", "", "", 405},
		{"follow method error", "GET", "/api/v1/accounts/test/follow", "http://localhost:5173", "", "", 405},
		{"denied read", "GET", "/api/v1/nope", "https://evil.test", "", "", 404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if tt.requestedMethod != "" {
				r.Header.Set("Access-Control-Request-Method", tt.requestedMethod)
			}
			if tt.headers != "" {
				r.Header.Set("Access-Control-Request-Headers", tt.headers)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "private, no-store" || !strings.Contains(strings.Join(w.Header().Values("Vary"), ","), "Origin") {
				t.Fatal("missing private/cache variance headers")
			}
			allowed := tt.origin == "http://localhost:5173"
			if (w.Header().Get("Access-Control-Allow-Origin") == tt.origin && tt.origin != "") != allowed {
				t.Fatal("incorrect origin reflection")
			}
			if allowed && (w.Header().Get("Access-Control-Allow-Credentials") != "true" || !strings.Contains(w.Header().Get("Access-Control-Expose-Headers"), "Retry-After")) {
				t.Fatal("missing credential/exposed headers")
			}
			if tt.method == "OPTIONS" && !strings.Contains(strings.Join(w.Header().Values("Vary"), ","), "Access-Control-Request-Headers") {
				t.Fatal("missing preflight variance")
			}
			if tt.status == 405 && w.Header().Get("Allow") == "" {
				t.Fatal("missing Allow on API method error")
			}
		})
	}
}

func TestCORSOnEarlyErrorsAndDuplicateOrigin(t *testing.T) {
	handler := NewHandler(unusedStore{}, []string{"http://localhost:5173"}, []byte(strings.Repeat("k", 32)), false, testLogger())
	r := httptest.NewRequest("POST", "/api/v1/auth/register", strings.NewReader(strings.Repeat(" ", maxBodyBytes+1)))
	r.Header.Set("Origin", "http://localhost:5173")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 413 || w.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Fatal("early error lost CORS")
	}
	r = httptest.NewRequest("POST", "/api/v1/auth/login", nil)
	r.Header.Add("Origin", "http://localhost:5173")
	r.Header.Add("Origin", "https://evil.test")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 403 || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("ambiguous origin accepted")
	}
}

func TestCookieAttributesAndCSRFDerivation(t *testing.T) {
	for _, secure := range []bool{false, true} {
		s := &server{secureCookies: secure, csrfSigningKey: []byte(strings.Repeat("k", 32))}
		for _, token := range []string{"session-token", ""} {
			w := httptest.NewRecorder()
			s.setSessionCookie(w, token)
			cookies := w.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatal("missing cookie")
			}
			c := cookies[0]
			if c.HttpOnly != true || c.Secure != secure || c.Domain != "" || c.Path != "/" || c.SameSite != http.SameSiteLaxMode || c.Value != token {
				t.Fatalf("incorrect cookie: %+v", c)
			}
			if secure && c.Name != "__Host-stacktrace_session" || !secure && c.Name != "stacktrace_session_dev" {
				t.Fatal("incorrect cookie name")
			}
			if token == "" && c.MaxAge != -1 || token != "" && c.MaxAge != 604800 {
				t.Fatal("incorrect expiry")
			}
		}
		first := s.csrfToken("one")
		if first != s.csrfToken("one") || first == s.csrfToken("two") {
			t.Fatal("CSRF must be stable and session-bound")
		}
		s.csrfSigningKey = []byte(strings.Repeat("z", 32))
		if first == s.csrfToken("one") {
			t.Fatal("CSRF not signing-key-bound")
		}
	}
}
