package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// Feed cursors have their own concrete contract. Reply/bookmark v1 payloads and
// their viewer-independent reply-preview continuation remain unchanged.
type feedCursorBinding struct {
	Scope  string       `json:"scope"`
	Target app.ID       `json:"target"`
	Viewer app.ID       `json:"viewer"`
	View   app.FeedView `json:"view"`
	Tag    string       `json:"tag"`
	Sort   app.FeedSort `json:"sort"`
}

type feedCursorPayload struct {
	Version int `json:"version"`
	feedCursorBinding
	Ceiling   string       `json:"ceiling"`
	Timestamp string       `json:"timestamp"`
	Kind      app.FeedKind `json:"kind"`
	ID        app.ID       `json:"id"`
	Score     int64        `json:"score"`
}

func (binding feedCursorBinding) valid() bool {
	if binding.Viewer != "" && !canonicalFeedID(binding.Viewer) {
		return false
	}
	switch binding.Scope {
	case "feed":
		query, err := (app.FeedQuery{View: binding.View, Sort: binding.Sort, Tag: binding.Tag}).Normalize()
		return err == nil && binding.Target == "" && query.View == binding.View && query.Sort == binding.Sort && query.Tag == binding.Tag && (binding.View != app.FeedViewFollowing || binding.Viewer != "")
	case "account-feed":
		return canonicalFeedID(binding.Target) && binding.View == "" && binding.Tag == "" && binding.Sort == app.FeedSortNewest
	default:
		return false
	}
}

func canonicalFeedID(id app.ID) bool {
	parsed, err := app.ParseID(string(id))
	return err == nil && parsed == id && id != "00000000-0000-0000-0000-000000000000"
}

func (payload feedCursorPayload) position() (app.FeedPosition, time.Time, error) {
	if payload.Version != 1 || !payload.feedCursorBinding.valid() || !canonicalFeedID(payload.ID) {
		return app.FeedPosition{}, time.Time{}, errInvalidCursor
	}
	ceiling, err := time.Parse(time.RFC3339Nano, payload.Ceiling)
	if err != nil || ceiling.IsZero() || payload.Ceiling != formatTimestamp(ceiling) {
		return app.FeedPosition{}, time.Time{}, errInvalidCursor
	}
	timestamp, err := time.Parse(time.RFC3339Nano, payload.Timestamp)
	if err != nil || timestamp.IsZero() || payload.Timestamp != formatTimestamp(timestamp) {
		return app.FeedPosition{}, time.Time{}, errInvalidCursor
	}
	position := app.FeedPosition{Timestamp: timestamp.UTC(), Kind: payload.Kind, ID: payload.ID, Score: payload.Score}
	if _, err := (app.FeedWindow{Position: &position, InitialCeiling: ceiling}).Normalize(payload.Sort); err != nil {
		return app.FeedPosition{}, time.Time{}, errInvalidCursor
	}
	return position, ceiling.UTC(), nil
}

func (s *server) encodeFeedCursor(binding feedCursorBinding, ceiling time.Time, position *app.FeedPosition) (*string, error) {
	if position == nil {
		return nil, nil
	}
	payload := feedCursorPayload{Version: 1, feedCursorBinding: binding, Ceiling: formatTimestamp(ceiling), Timestamp: formatTimestamp(position.Timestamp), Kind: position.Kind, ID: position.ID, Score: position.Score}
	if _, _, err := payload.position(); err != nil {
		return nil, err
	}
	contents, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, s.cursorSigningKey)
	_, _ = mac.Write(contents)
	token := base64.RawURLEncoding.EncodeToString(contents) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if len(token) > maxCursorBytes {
		return nil, errInvalidCursor
	}
	return &token, nil
}

func (s *server) decodeFeedCursor(token string, binding feedCursorBinding) (app.FeedPosition, time.Time, error) {
	if token == "" || len(token) > maxCursorBytes || strings.Count(token, ".") != 1 {
		return app.FeedPosition{}, time.Time{}, errInvalidCursor
	}
	parts := strings.SplitN(token, ".", 2)
	contents, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || base64.RawURLEncoding.EncodeToString(contents) != parts[0] {
		return app.FeedPosition{}, time.Time{}, errInvalidCursor
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(signature) != sha256.Size || base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
		return app.FeedPosition{}, time.Time{}, errInvalidCursor
	}
	mac := hmac.New(sha256.New, s.cursorSigningKey)
	_, _ = mac.Write(contents)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return app.FeedPosition{}, time.Time{}, errInvalidCursor
	}
	var payload feedCursorPayload
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return app.FeedPosition{}, time.Time{}, errInvalidCursor
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return app.FeedPosition{}, time.Time{}, errInvalidCursor
	}
	canonical, err := json.Marshal(payload)
	if err != nil || !bytes.Equal(contents, canonical) || payload.feedCursorBinding != binding {
		return app.FeedPosition{}, time.Time{}, errInvalidCursor
	}
	return payload.position()
}
