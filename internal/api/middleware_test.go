package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRecoveryAndSafeRequestLogs(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	server := newServer(func(context.Context) error { return nil }, logger)
	server.mux.HandleFunc("POST /api/v1/failure/{id}", func(http.ResponseWriter, *http.Request) { panic("secret panic details") })
	request := httptest.NewRequest("POST", "/api/v1/failure/secret-path?password=secret-query", strings.NewReader("secret-body"))
	request.Header.Set("Cookie", "session=secret-cookie")
	request.Header.Set("Authorization", "Bearer secret-token")
	response := httptest.NewRecorder()
	server.handler().ServeHTTP(response, request)
	if response.Code != 500 || !json.Valid(response.Body.Bytes()) {
		t.Fatalf("panic response: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(logs.String(), "secret") || strings.Contains(response.Body.String(), "secret") {
		t.Fatal("recovery disclosed request or panic contents")
	}
	var record map[string]any
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["route"] != "POST /api/v1/failure/{id}" || record["status"] != float64(500) || record["failure"] != "panic" || record["request_id"] != response.Header().Get("X-Request-ID") || record["duration_ms"] == nil {
		t.Fatalf("incomplete request log: %v", record)
	}
}

func TestPanicAfterWriteAbortsWithoutAppendingError(t *testing.T) {
	server := newServer(func(context.Context) error { return nil }, testLogger())
	server.mux.HandleFunc("GET /api/v1/partial", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("secret")
	})
	response := httptest.NewRecorder()
	defer func() {
		if recover() != http.ErrAbortHandler || response.Body.String() != "partial" {
			t.Fatal("partial response was not aborted cleanly")
		}
	}()
	server.handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/partial", nil))
}

func TestRequestDeadline(t *testing.T) {
	server := newServer(func(context.Context) error { return nil }, testLogger())
	server.mux.HandleFunc("GET /api/v1/deadline", func(w http.ResponseWriter, r *http.Request) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > requestTimeout {
			t.Error("request deadline is missing or exceeds the fixed timeout")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	server.handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/deadline", nil))
	server.mux.HandleFunc("GET /api/v1/slow", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		writeFailure(w, r.Context().Err())
	})
	response := httptest.NewRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	server.handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/slow", nil).WithContext(ctx))
	if response.Code != 503 || !strings.Contains(response.Body.String(), "request_timeout") {
		t.Fatalf("deadline contract failed: %d %s", response.Code, response.Body.String())
	}
}

func TestRateLimitsUseConnectionIP(t *testing.T) {
	server := newServer(func(context.Context) error { return nil }, testLogger())
	handler := server.handler()
	request := func(peer, forwarded, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = peer
		r.Header.Set("X-Forwarded-For", forwarded)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		return response
	}
	for range server.limiter.requests {
		if request("192.0.2.1:1234", "198.51.100.1", "/unknown").Code != 404 {
			t.Fatal("request was denied before reaching the quota")
		}
	}
	limited := request("192.0.2.1:5678", "198.51.100.2", "/unknown")
	if limited.Code != 429 || limited.Header().Get("Retry-After") == "" || !json.Valid(limited.Body.Bytes()) {
		t.Fatal("changed port or forwarded IP bypassed limit or missing retry response")
	}
	if request("192.0.2.1:1234", "", "/healthz").Code != 200 || request("192.0.2.1:1234", "", "/readyz").Code != 200 {
		t.Fatal("API quota should not consume probe availability")
	}
	if request("[::ffff:192.0.2.1]:1234", "", "/unknown").Code != 429 {
		t.Fatal("mapped IPv4 address bypassed limit")
	}
	if request("192.0.2.2:1234", "192.0.2.1", "/unknown").Code != 404 {
		t.Fatal("different connection IP should have a separate bucket")
	}
}

func TestMethodPatternsAndFileRejection(t *testing.T) {
	server := newServer(func(context.Context) error { return nil }, testLogger())
	server.mux.HandleFunc("POST /api/v1/resources/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"id": r.PathValue("id")})
	})
	server.mux.HandleFunc("DELETE /api/v1/resources/{id}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	server.mux.HandleFunc("/api/v1/resources/{id}", methodNotAllowed("DELETE, POST"))
	handler := server.handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("PUT", "/api/v1/resources/123", nil))
	if response.Code != 405 || response.Header().Get("Allow") != "DELETE, POST" {
		t.Fatal("method-aware route fallback lost Allow")
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/api/v1/resources/123", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"id":"123"`) {
		t.Fatal("ServeMux PathValue was not populated")
	}
	for _, path := range []string{"/", "/go.mod", "/.git/config", "/assets/app.js", "/index.html", "/docs/", "/healthz/", "/healthz/../.env", "//healthz", "/%2e%2e/.env"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 404 || !json.Valid(response.Body.Bytes()) || response.Header().Get("Location") != "" {
			t.Errorf("%s: expected JSON 404 without redirect, got %d", path, response.Code)
		}
	}
}

func TestChunkedOversizedHTTPBody(t *testing.T) {
	server := httptest.NewServer(decodingHandler())
	defer server.Close()
	request, err := http.NewRequest("POST", server.URL+"/api/v1/test", io.NopCloser(strings.NewReader(`{"name":"`+strings.Repeat("a", maxBodyBytes)+`"}`)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 413 {
		t.Fatalf("chunked body: got %d", response.StatusCode)
	}
}
