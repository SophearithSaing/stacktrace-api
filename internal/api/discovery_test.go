package api

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestSearchCursorBindsViewerAndQuery(t *testing.T) {
	s := newServer(nil, nil)
	s.cursorSigningKey = []byte("search cursor test key")
	viewer := app.ID("00000000-0000-0000-0000-000000000001")
	position := &app.SearchPosition{CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ID: app.ID("00000000-0000-0000-0000-000000000002")}
	token, err := s.encodeSearchCursor(viewer, "jsonb", position.CreatedAt, position)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.decodeSearchCursor(*token, viewer, "jsonb"); err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if _, _, err := s.decodeSearchCursor(*token, viewer, "other"); err == nil {
		t.Fatal("cursor accepted another query")
	}
	if _, _, err := s.decodeSearchCursor(*token, app.ID("00000000-0000-0000-0000-000000000003"), "jsonb"); err == nil {
		t.Fatal("cursor accepted another viewer")
	}
}

func TestSearchCursorRejectsMalformedSignedPayloads(t *testing.T) {
	s := &server{cursorSigningKey: []byte("search cursor test key")}
	valid := searchCursorPayload{1, "search-posts-newest", "", "q", "2026-01-01T00:00:00Z", "2025-12-31T00:00:00Z", "00000000-0000-0000-0000-000000000001"}
	for _, change := range []func(*searchCursorPayload){func(p *searchCursorPayload) { p.Version = 2 }, func(p *searchCursorPayload) { p.Scope = "other" }, func(p *searchCursorPayload) { p.Ceiling = "bad" }, func(p *searchCursorPayload) { p.Position = "bad" }, func(p *searchCursorPayload) { p.ID = "bad" }, func(p *searchCursorPayload) { p.ID = "00000000-0000-0000-0000-000000000000" }} {
		payload := valid
		change(&payload)
		contents, _ := json.Marshal(payload)
		if _, _, err := s.decodeSearchCursor(signRawTestCursor(s.cursorSigningKey, contents), "", "query"); !errors.Is(err, errInvalidCursor) {
			t.Fatalf("accepted %+v", payload)
		}
	}
	if _, _, err := s.decodeSearchCursor(strings.Repeat("x", maxCursorBytes+1), "", "query"); !errors.Is(err, errInvalidCursor) {
		t.Fatal("accepted oversize cursor")
	}
}
