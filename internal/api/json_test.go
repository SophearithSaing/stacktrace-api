package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

type trackedBody struct {
	io.Reader
	closeCalls int
}

func (body *trackedBody) Close() error { body.closeCalls++; return nil }

func decodingHandler() http.Handler {
	server := newServer(func(context.Context) error { return nil }, testLogger())
	server.mux.HandleFunc("POST /api/v1/test", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Name string `json:"name"`
		}
		if err := decodeJSON(r, &request); err != nil {
			writeFailure(w, err)
			return
		}
		if request.Name == "" {
			writeFailure(w, &app.ValidationError{Fields: map[string]string{"name": "Required"}})
			return
		}
		writeJSON(w, http.StatusOK, request)
	})
	return server.handler()
}

func TestJSONDecoding(t *testing.T) {
	for _, test := range []struct {
		name, body, contentType string
		status                  int
	}{
		{"object", `{"name":"Go"}`, "application/json", 200},
		{"UTF8 charset", `{"name":"Go"}`, "application/json; charset=UTF-8", 200},
		{"missing type", `{"name":"Go"}`, "", 415},
		{"wrong type", `{"name":"Go"}`, "text/plain", 415},
		{"malformed type", `{}`, "application/json;broken", 415},
		{"wrong charset", `{}`, "application/json; charset=latin1", 415},
		{"empty", "", "application/json", 400},
		{"null", `null`, "application/json", 400},
		{"array", `[]`, "application/json", 400},
		{"scalar", `"Go"`, "application/json", 400},
		{"unknown field", `{"name":"Go","author_id":"secret"}`, "application/json", 400},
		{"wrong field type", `{"name":123}`, "application/json", 400},
		{"truncated", `{"name":`, "application/json", 400},
		{"second object", `{"name":"Go"} {}`, "application/json", 400},
		{"trailing junk", `{"name":"Go"} secret`, "application/json", 400},
		{"whitespace", " \n{\"name\":\"Go\"}\t\r\n", "application/json", 200},
		{"non-JSON whitespace", "\u00a0{\"name\":\"Go\"}", "application/json", 400},
		{"invalid UTF8", "{\"name\":\"\xff\"}", "application/json", 400},
		{"domain validation", `{}`, "application/json", 422},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &trackedBody{Reader: strings.NewReader(test.body)}
			request := httptest.NewRequest("POST", "/api/v1/test", body)
			request.Header.Set("Content-Type", test.contentType)
			response := httptest.NewRecorder()
			decodingHandler().ServeHTTP(response, request)
			if response.Code != test.status || body.closeCalls != 1 {
				t.Fatalf("status=%d body close calls=%d; want %d and one close", response.Code, body.closeCalls, test.status)
			}
			if !json.Valid(response.Body.Bytes()) || strings.Contains(response.Body.String(), "secret") {
				t.Fatal("invalid JSON or leaked input")
			}
			if test.status == 422 {
				var failure errorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.Error.Fields["name"] != "Required" {
					t.Fatal("missing field validation error")
				}
			}
		})
	}
}

func TestBodyLimits(t *testing.T) {
	for _, knownLength := range []bool{true, false} {
		for _, size := range []int{maxBodyBytes, maxBodyBytes + 1, 2 * maxBodyBytes} {
			t.Run(fmt.Sprintf("length_known=%v/size=%d", knownLength, size), func(t *testing.T) {
				// Oversized trailing whitespace counts against the complete body limit.
				contents := `{"name":"Go"}` + strings.Repeat(" ", size-len(`{"name":"Go"}`))
				body := &trackedBody{Reader: strings.NewReader(contents)}
				request := httptest.NewRequest("POST", "/api/v1/test", body)
				if knownLength {
					request.ContentLength = int64(size)
				} else {
					request.ContentLength = -1
				}
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				decodingHandler().ServeHTTP(response, request)
				want := 200
				if size > maxBodyBytes {
					want = 413
				}
				if response.Code != want || body.closeCalls != 1 {
					t.Fatalf("status=%d body close calls=%d; want %d and one close", response.Code, body.closeCalls, want)
				}
			})
		}
	}
}

func TestFailureContract(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
	}{
		{app.ErrInvalidID, 400}, {app.ErrUnauthenticated, 401}, {app.ErrForbidden, 403},
		{app.ErrNotFound, 404}, {app.ErrConflict, 409}, {app.ErrDeleted, 410},
		{app.ErrInvalidHandle, 422}, {app.ErrUnavailable, 503},
		{context.DeadlineExceeded, 503}, {context.Canceled, 503}, {errors.New("secret SQL error"), 500},
	} {
		response := httptest.NewRecorder()
		response.Header().Set("X-Request-ID", "test-id")
		writeFailure(response, fmt.Errorf("secret wrapper: %w", test.err))
		var body errorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != test.status || body.RequestID != "test-id" || body.Error.Code == "" || strings.Contains(response.Body.String(), "secret") {
			t.Fatalf("unsafe or incorrect error contract: status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestJSONEncodingFailureIsAtomic(t *testing.T) {
	response := httptest.NewRecorder()
	writeJSON(response, http.StatusOK, make(chan int))
	if response.Code != 500 || !json.Valid(response.Body.Bytes()) {
		t.Fatalf("encoding failure committed partial success: %d %s", response.Code, response.Body.String())
	}
}
