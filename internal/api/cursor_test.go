package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestCursorRoundTripAndBindings(t *testing.T) {
	s := &server{cursorSigningKey: []byte(strings.Repeat("c", 32))}
	target, viewer, positionID := app.NewID(), app.NewID(), app.NewID()
	ceiling := time.Date(2026, 9, 18, 12, 0, 0, 123, time.UTC)
	position := &app.KeysetPosition{Timestamp: ceiling.Add(-time.Second), ID: positionID}
	token, err := s.encodeCursor("replies", target, "", app.ReplySortOldest, ceiling, position)
	if err != nil || token == nil {
		t.Fatal("cursor was not encoded", err)
	}
	got, gotCeiling, err := s.decodeCursor(*token, "replies", target, "", app.ReplySortOldest)
	if err != nil || got.ID != positionID || !got.Timestamp.Equal(position.Timestamp) || !gotCeiling.Equal(ceiling) {
		t.Fatalf("round trip failed: %+v %v %v", got, gotCeiling, err)
	}
	bad := []func() error{
		func() error {
			_, _, err := s.decodeCursor(*token+"x", "replies", target, "", app.ReplySortOldest)
			return err
		},
		func() error {
			_, _, err := s.decodeCursor(*token, "bookmarks", "", viewer, app.ReplySortNewest)
			return err
		},
		func() error {
			_, _, err := s.decodeCursor(*token, "replies", app.NewID(), "", app.ReplySortOldest)
			return err
		},
		func() error {
			_, _, err := s.decodeCursor(*token, "replies", target, "", app.ReplySortNewest)
			return err
		},
		func() error {
			other := &server{cursorSigningKey: []byte(strings.Repeat("d", 32))}
			_, _, err := other.decodeCursor(*token, "replies", target, "", app.ReplySortOldest)
			return err
		},
		func() error {
			_, _, err := s.decodeCursor(strings.Repeat("x", maxCursorBytes+1), "replies", target, "", app.ReplySortOldest)
			return err
		},
	}
	for i, check := range bad {
		if err := check(); err == nil {
			t.Fatalf("invalid cursor case %d accepted", i)
		}
	}
	bookmarkToken, err := s.encodeCursor("bookmarks", "", viewer, app.ReplySortNewest, ceiling, position)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.decodeCursor(*bookmarkToken, "bookmarks", "", app.NewID(), app.ReplySortNewest); err == nil {
		t.Fatal("cursor accepted for another viewer")
	}
	for _, payload := range []cursorPayload{
		{Version: 2, Kind: "replies", Target: string(target), Sort: string(app.ReplySortOldest), Ceiling: ceiling.Format(time.RFC3339Nano), Position: position.Timestamp.Format(time.RFC3339Nano), ID: string(positionID)},
		{Version: 1, Kind: "replies", Target: string(target), Sort: string(app.ReplySortOldest), Ceiling: ceiling.Format(time.RFC3339Nano), Position: ceiling.Add(time.Second).Format(time.RFC3339Nano), ID: string(positionID)},
	} {
		if _, _, err := s.decodeCursor(signTestCursor(t, s.cursorSigningKey, payload), "replies", target, "", app.ReplySortOldest); err == nil {
			t.Fatal("accepted invalid signed payload")
		}
	}
}

func TestCursorRejectsMalformedSignedPayloads(t *testing.T) {
	s := &server{cursorSigningKey: []byte(strings.Repeat("c", 32))}
	target, positionID := app.NewID(), app.NewID()
	ceiling := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	valid := cursorPayload{Version: 1, Kind: "replies", Target: string(target), Sort: string(app.ReplySortOldest), Ceiling: ceiling.Format(time.RFC3339Nano), Position: ceiling.Format(time.RFC3339Nano), ID: string(positionID)}

	for _, payload := range []cursorPayload{
		{Version: 1, Kind: "replies", Target: "not-an-id", Sort: string(app.ReplySortOldest), Ceiling: valid.Ceiling, Position: valid.Position, ID: valid.ID},
		{Version: 1, Kind: "replies", Target: valid.Target, Sort: valid.Sort, Ceiling: "0001-01-01T00:00:00Z", Position: valid.Position, ID: valid.ID},
		{Version: 1, Kind: "replies", Target: valid.Target, Sort: valid.Sort, Ceiling: "not-a-time", Position: valid.Position, ID: valid.ID},
		{Version: 1, Kind: "replies", Target: valid.Target, Sort: valid.Sort, Ceiling: valid.Ceiling, Position: ceiling.Add(time.Nanosecond).Format(time.RFC3339Nano), ID: valid.ID},
		{Version: 1, Kind: "replies", Target: valid.Target, Sort: valid.Sort, Ceiling: valid.Ceiling, Position: valid.Position, ID: "00000000-0000-0000-0000-000000000000"},
		{Version: 1, Kind: "replies", Target: valid.Target, Sort: valid.Sort, Ceiling: valid.Ceiling, Position: valid.Position, ID: "not-an-id"},
	} {
		if _, _, err := s.decodeCursor(signTestCursor(t, s.cursorSigningKey, payload), "replies", target, "", app.ReplySortOldest); err == nil {
			t.Fatalf("accepted malformed payload: %+v", payload)
		}
	}

	for _, contents := range []string{
		`{"version":1,"kind":"replies","target":"` + string(target) + `","sort":"oldest","ceiling":"2026-09-18T12:00:00Z","position":"2026-09-18T12:00:00Z","id":"` + string(positionID) + `","extra":true}`,
		`{"version":1,"version":1,"kind":"replies","target":"` + string(target) + `","sort":"oldest","ceiling":"2026-09-18T12:00:00Z","position":"2026-09-18T12:00:00Z","id":"` + string(positionID) + `"}`,
		`{"version":1,"kind":"replies","target":"` + string(target) + `","sort":"oldest","ceiling":"2026-09-18T12:00:00Z","position":"2026-09-18T12:00:00Z","id":"` + string(positionID) + `"} trailing`,
	} {
		if _, _, err := s.decodeCursor(signRawTestCursor(s.cursorSigningKey, []byte(contents)), "replies", target, "", app.ReplySortOldest); err == nil {
			t.Fatalf("accepted noncanonical JSON %q", contents)
		}
	}
	for _, token := range []string{"", "not-base64.sig", "e30", "e30.", ".", "e30..sig", strings.Repeat("a", maxCursorBytes+1)} {
		if _, _, err := s.decodeCursor(token, "replies", target, "", app.ReplySortOldest); err == nil {
			t.Fatalf("accepted malformed token %q", token)
		}
	}
}

func signTestCursor(t *testing.T, key []byte, payload cursorPayload) string {
	t.Helper()
	contents, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return signRawTestCursor(key, contents)
}

func signRawTestCursor(key, contents []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(contents)
	return base64.RawURLEncoding.EncodeToString(contents) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestStrictPaginationQuery(t *testing.T) {
	for _, raw := range []string{"limit=0", "limit=51", "limit=x", "limit=1&limit=2", "limit=", "unknown=x", "cursor=%zz", "sort=", "a=b;c=d"} {
		values, err := strictQuery(raw, "limit", "cursor", "sort")
		if err == nil {
			s := &server{cursorSigningKey: []byte(strings.Repeat("c", 32))}
			_, err = s.readWindow(values, "replies", app.NewID(), "", app.ReplySortOldest)
		}
		if err == nil {
			t.Fatalf("accepted query %q", raw)
		}
	}
}
