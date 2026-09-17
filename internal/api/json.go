package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

type errorResponse struct {
	Error     apiError `json:"error"`
	RequestID string   `json:"request_id"`
}

type apiError struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

type requestError struct {
	status        int
	code, message string
}

func (e *requestError) Error() string { return e.message }

// decodeJSON validates and decodes one UTF-8 JSON object. The API middleware
// owns the body byte limit and closure; domain validation follows decoding.
func decodeJSON(r *http.Request, destination any) error {
	mediaType, parameters, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || (parameters["charset"] != "" && !strings.EqualFold(parameters["charset"], "utf-8")) {
		return &requestError{http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json with UTF-8 encoding"}
	}
	if encoding := r.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return &requestError{http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Encoding is not supported"}
	}
	contents, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return &requestError{http.StatusRequestEntityTooLarge, "body_too_large", "Request body exceeds the byte limit"}
		}
		if r.Context().Err() != nil {
			return r.Context().Err()
		}
		return &requestError{http.StatusBadRequest, "invalid_json", "Could not read the JSON body"}
	}
	contents = bytes.Trim(contents, " \t\r\n")
	if len(contents) == 0 || contents[0] != '{' || !utf8.Valid(contents) {
		return &requestError{http.StatusBadRequest, "invalid_json", "Body must be a single UTF-8 JSON object"}
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return &requestError{http.StatusBadRequest, "invalid_json", "JSON is malformed or contains unknown fields or invalid field types"}
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return &requestError{http.StatusBadRequest, "invalid_json", "Body must contain only one JSON object"}
	}
	return nil
}

func writeFailure(w http.ResponseWriter, err error) {
	var transport *requestError
	var validation *app.ValidationError
	switch {
	case errors.As(err, &transport):
		writeError(w, transport.status, transport.code, transport.message)
	case errors.As(err, &validation):
		writeErrorFields(w, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", validation.Fields)
	case errors.Is(err, app.ErrInvalidID):
		writeError(w, http.StatusBadRequest, "invalid_id", "Invalid resource ID")
	case errors.Is(err, app.ErrInvalidHandle):
		writeErrorFields(w, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", map[string]string{"handle": app.ErrInvalidHandle.Error()})
	case errors.Is(err, app.ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Authentication is required")
	case errors.Is(err, app.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "Permission denied")
	case errors.Is(err, app.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "Resource not found")
	case errors.Is(err, app.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "Resource conflicts with an existing operation")
	case errors.Is(err, app.ErrDeleted):
		writeError(w, http.StatusGone, "deleted", "Resource has been deleted")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusServiceUnavailable, "request_timeout", "Request deadline exceeded")
	case errors.Is(err, context.Canceled):
		writeError(w, http.StatusServiceUnavailable, "request_cancelled", "Request was cancelled")
	case errors.Is(err, app.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Service is unavailable")
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "Internal server error")
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeErrorFields(w, status, code, message, nil)
}

func writeErrorFields(w http.ResponseWriter, status int, code, message string, fields map[string]string) {
	if recorder, ok := w.(*responseRecorder); ok {
		recorder.failure = code
	}
	writeJSON(w, status, errorResponse{
		Error:     apiError{Code: code, Message: message, Fields: fields},
		RequestID: w.Header().Get("X-Request-ID"),
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	contents, err := json.Marshal(value)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Internal server error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(contents, '\n'))
}
