package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

const maxCursorBytes = 2048

var errInvalidCursor = errors.New("invalid cursor")

type cursorPayload struct {
	Version  int    `json:"version"`
	Kind     string `json:"kind"`
	Target   string `json:"target"`
	Viewer   string `json:"viewer"`
	Sort     string `json:"sort"`
	Ceiling  string `json:"ceiling"`
	Position string `json:"position"`
	ID       string `json:"id"`
}

func (s *server) encodeCursor(kind string, target, viewer app.ID, sort app.ReplySort, ceiling time.Time, position *app.KeysetPosition) (*string, error) {
	if position == nil {
		return nil, nil
	}
	payload := cursorPayload{1, kind, string(target), string(viewer), string(sort), ceiling.UTC().Format(time.RFC3339Nano), position.Timestamp.UTC().Format(time.RFC3339Nano), string(position.ID)}
	contents, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, s.cursorSigningKey)
	_, _ = mac.Write(contents)
	token := base64.RawURLEncoding.EncodeToString(contents) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return &token, nil
}

func (s *server) decodeCursor(token, kind string, target, viewer app.ID, sort app.ReplySort) (app.KeysetPosition, time.Time, error) {
	if token == "" || len(token) > maxCursorBytes || strings.Count(token, ".") != 1 {
		return app.KeysetPosition{}, time.Time{}, errInvalidCursor
	}
	parts := strings.SplitN(token, ".", 2)
	contents, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || base64.RawURLEncoding.EncodeToString(contents) != parts[0] {
		return app.KeysetPosition{}, time.Time{}, errInvalidCursor
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(signature) != sha256.Size || base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
		return app.KeysetPosition{}, time.Time{}, errInvalidCursor
	}
	mac := hmac.New(sha256.New, s.cursorSigningKey)
	_, _ = mac.Write(contents)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return app.KeysetPosition{}, time.Time{}, errInvalidCursor
	}
	var payload cursorPayload
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return app.KeysetPosition{}, time.Time{}, errInvalidCursor
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return app.KeysetPosition{}, time.Time{}, errInvalidCursor
	}
	canonical, err := json.Marshal(payload)
	if err != nil || !bytes.Equal(contents, canonical) {
		return app.KeysetPosition{}, time.Time{}, errInvalidCursor
	}
	if payload.Version != 1 || payload.Kind != kind || payload.Target != string(target) || payload.Viewer != string(viewer) || payload.Sort != string(sort) {
		return app.KeysetPosition{}, time.Time{}, errInvalidCursor
	}
	ceiling, err := time.Parse(time.RFC3339Nano, payload.Ceiling)
	if err != nil || ceiling.IsZero() || payload.Ceiling != ceiling.UTC().Format(time.RFC3339Nano) {
		return app.KeysetPosition{}, time.Time{}, errInvalidCursor
	}
	positionTime, err := time.Parse(time.RFC3339Nano, payload.Position)
	if err != nil || positionTime.IsZero() || positionTime.After(ceiling) || payload.Position != positionTime.UTC().Format(time.RFC3339Nano) {
		return app.KeysetPosition{}, time.Time{}, errInvalidCursor
	}
	id, err := app.ParseID(payload.ID)
	if err != nil || id == "00000000-0000-0000-0000-000000000000" || payload.ID != string(id) {
		return app.KeysetPosition{}, time.Time{}, errInvalidCursor
	}
	return app.KeysetPosition{Timestamp: positionTime.UTC(), ID: id}, ceiling.UTC(), nil
}

func invalidCursorError() error {
	return &requestError{status: 400, code: "invalid_cursor", message: "Cursor is invalid"}
}

func invalidQueryError() error {
	return &requestError{status: 400, code: "invalid_query", message: "Query parameters are invalid"}
}

func cursorError(err error) error {
	if errors.Is(err, errInvalidCursor) {
		return invalidCursorError()
	}
	return fmt.Errorf("cursor: %w", err)
}
