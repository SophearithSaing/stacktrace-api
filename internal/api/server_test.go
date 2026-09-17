package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestInfrastructureRoutes(t *testing.T) {
	for _, tt := range []struct {
		name, method, path string
		databaseError      error
		wantStatus         int
		wantCode           string
		wantPing           bool
	}{
		{"liveness ignores database", "GET", "/healthz", errors.New("offline"), 200, "", false},
		{"ready", "GET", "/readyz", nil, 200, "", true},
		{"unavailable", "GET", "/readyz", errors.New("secret connection details"), 503, "unavailable", true},
		{"root", "GET", "/", nil, 404, "not_found", false},
		{"unknown API", "GET", "/api/v1/posts", nil, 404, "not_found", false},
		{"file-like path", "GET", "/.env", nil, 404, "not_found", false},
		{"documentation path", "GET", "/docs/backend-plan.research.md", nil, 404, "not_found", false},
		{"unknown method and path", "POST", "/unknown", nil, 404, "not_found", false},
		{"wrong health method", "POST", "/healthz", nil, 405, "method_not_allowed", false},
		{"wrong ready method", "DELETE", "/readyz", nil, 405, "method_not_allowed", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			handler := NewHandler(func(ctx context.Context) error {
				called = true
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 2*time.Second {
					t.Error("readiness query is not deadline bounded")
				}
				return tt.databaseError
			}, testLogger())
			w := httptest.NewRecorder()
			r := httptest.NewRequest(tt.method, tt.path, nil)
			r.Header.Set("X-Request-ID", "untrusted-client-value")
			handler.ServeHTTP(w, r)
			if w.Code != tt.wantStatus || called != tt.wantPing {
				t.Fatalf("status=%d ping=%v; want status=%d ping=%v", w.Code, called, tt.wantStatus, tt.wantPing)
			}
			if w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing JSON or cache headers")
			}
			requestID := w.Header().Get("X-Request-ID")
			if requestID == "" || requestID == "untrusted-client-value" {
				t.Fatal("request ID was not server-generated")
			}
			if tt.wantStatus == 405 && w.Header().Get("Allow") != "GET, HEAD" {
				t.Fatal("incorrect Allow header")
			}
			if !json.Valid(w.Body.Bytes()) || strings.Contains(w.Body.String(), "secret") {
				t.Fatal("invalid or unsafe response")
			}
			if tt.wantCode != "" {
				var body errorResponse
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.Error.Code != tt.wantCode || body.Error.Message == "" || body.RequestID != requestID {
					t.Fatalf("incorrect error response: %+v", body)
				}
			}
		})
	}
}

func TestCancelledRequestDoesNotStartReadinessQuery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handler := NewHandler(func(ctx context.Context) error {
		t.Error("cancelled request started a database query")
		return nil
	}, testLogger())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil).WithContext(ctx))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d; want 503", w.Code)
	}
}

func TestHeadHasNoResponseBody(t *testing.T) {
	server := httptest.NewServer(NewHandler(func(context.Context) error { return nil }, testLogger()))
	defer server.Close()
	resp, err := server.Client().Head(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || len(body) != 0 {
		t.Fatalf("HEAD status=%d body length=%d error=%v", resp.StatusCode, len(body), err)
	}
}
