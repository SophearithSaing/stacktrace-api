package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestInfrastructureProbes(t *testing.T) {
	ready := false
	handler := (&Infrastructure{
		Ready: func(ctx context.Context) bool {
			return ready
		},
	}).Handler()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status %d", rec.Code)
	}

	ready = true
	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz status %d", rec.Code)
	}
}

func TestInfrastructureRejectsUnknownRoutes(t *testing.T) {
	handler := (&Infrastructure{Ready: func(context.Context) bool { return true }}).Handler()
	for _, path := range []string{"/", "/api/v1/me", "/metrics"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("path %s status %d", path, rec.Code)
		}
	}
}

func TestInfrastructureReadinessUsesTimeout(t *testing.T) {
	handler := (&Infrastructure{
		Ready: func(ctx context.Context) bool {
			select {
			case <-time.After(10 * time.Second):
				return true
			case <-ctx.Done():
				return false
			}
		},
	}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	start := time.Now()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status %d", rec.Code)
	}
	if time.Since(start) > 7*time.Second {
		t.Fatal("readiness timeout too long")
	}
}
