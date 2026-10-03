package api

import (
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
