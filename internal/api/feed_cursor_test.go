package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestFeedCursorRoundTripAndBindings(t *testing.T) {
	s := &server{cursorSigningKey: []byte(strings.Repeat("c", 32))}
	ceiling := time.Date(2026, 9, 19, 12, 0, 0, 123456, time.UTC)
	viewer, target := app.NewID(), app.NewID()
	bindings := []feedCursorBinding{{Scope: "account-feed", Target: target, Sort: app.FeedSortNewest}, {Scope: "account-feed", Target: target, Viewer: viewer, Sort: app.FeedSortNewest}}
	for _, view := range []app.FeedView{app.FeedViewForYou, app.FeedViewFollowing, app.FeedViewSpicy} {
		for _, sort := range []app.FeedSort{app.FeedSortNewest, app.FeedSortRelevant, app.FeedSortReacted} {
			bindings = append(bindings, feedCursorBinding{Scope: "feed", Viewer: viewer, View: view, Sort: sort, Tag: "go_123"})
		}
	}
	bindings = append(bindings, feedCursorBinding{Scope: "feed", View: app.FeedViewForYou, Sort: app.FeedSortNewest})
	for _, binding := range bindings {
		for _, kind := range []app.FeedKind{app.FeedKindPost, app.FeedKindRepost} {
			position := app.FeedPosition{Timestamp: ceiling.Add(-time.Second), Kind: kind, ID: app.NewID()}
			if binding.Sort == app.FeedSortReacted {
				position.Score = math.MaxInt64
			}
			token, err := s.encodeFeedCursor(binding, ceiling, &position)
			if err != nil || token == nil || len(*token) > maxCursorBytes {
				t.Fatalf("encode=%v err=%v", token, err)
			}
			got, gotCeiling, err := s.decodeFeedCursor(*token, binding)
			if err != nil || got != position || gotCeiling != ceiling {
				t.Fatalf("position=%+v ceiling=%v err=%v", got, gotCeiling, err)
			}
			for _, change := range []func(*feedCursorBinding){
				func(b *feedCursorBinding) { b.Scope = "replies" },
				func(b *feedCursorBinding) { b.Target = app.NewID() },
				func(b *feedCursorBinding) { b.Viewer = app.NewID() },
				func(b *feedCursorBinding) { b.View = "other" },
				func(b *feedCursorBinding) { b.Tag = "rust" },
				func(b *feedCursorBinding) { b.Sort = "other" },
			} {
				other := binding
				change(&other)
				if _, _, err := s.decodeFeedCursor(*token, other); !errors.Is(err, errInvalidCursor) {
					t.Fatalf("accepted changed binding %+v", other)
				}
			}
			other := &server{cursorSigningKey: []byte(strings.Repeat("d", 32))}
			if _, _, err := other.decodeFeedCursor(*token, binding); !errors.Is(err, errInvalidCursor) {
				t.Fatal("accepted different signing key")
			}
		}
	}
	if token, err := s.encodeFeedCursor(bindings[0], ceiling, nil); token != nil || err != nil {
		t.Fatalf("empty cursor=%v err=%v", token, err)
	}
}

func TestFeedCursorRejectsInvalidSignedPayloads(t *testing.T) {
	s := &server{cursorSigningKey: []byte(strings.Repeat("c", 32))}
	valid := feedCursorPayload{Version: 1, feedCursorBinding: feedCursorBinding{Scope: "feed", View: app.FeedViewForYou, Sort: app.FeedSortNewest}, Ceiling: "2026-09-19T12:00:00Z", Timestamp: "2026-09-19T11:00:00Z", Kind: app.FeedKindRepost, ID: app.ID("abcdef01-1234-5678-abcd-0123456789ab")}
	for name, change := range map[string]func(*feedCursorPayload){
		"version":           func(p *feedCursorPayload) { p.Version = 2 },
		"scope":             func(p *feedCursorPayload) { p.Scope = "unknown" },
		"global target":     func(p *feedCursorPayload) { p.Target = app.NewID() },
		"account no target": func(p *feedCursorPayload) { p.Scope = "account-feed"; p.View = "" },
		"account view":      func(p *feedCursorPayload) { p.Scope = "account-feed"; p.Target = app.NewID() },
		"account tag": func(p *feedCursorPayload) {
			p.Scope = "account-feed"
			p.Target = app.NewID()
			p.View = ""
			p.Tag = "go"
		},
		"account sort": func(p *feedCursorPayload) {
			p.Scope = "account-feed"
			p.Target = app.NewID()
			p.View = ""
			p.Sort = app.FeedSortRelevant
		},
		"viewer":          func(p *feedCursorPayload) { p.Viewer = "bad" },
		"zero viewer":     func(p *feedCursorPayload) { p.Viewer = "00000000-0000-0000-0000-000000000000" },
		"following anon":  func(p *feedCursorPayload) { p.View = app.FeedViewFollowing },
		"view":            func(p *feedCursorPayload) { p.View = "unknown" },
		"default view":    func(p *feedCursorPayload) { p.View = "" },
		"sort":            func(p *feedCursorPayload) { p.Sort = "unknown" },
		"default sort":    func(p *feedCursorPayload) { p.Sort = "" },
		"tag":             func(p *feedCursorPayload) { p.Tag = "Go" },
		"long tag":        func(p *feedCursorPayload) { p.Tag = strings.Repeat("x", 65) },
		"bad tag":         func(p *feedCursorPayload) { p.Tag = "#go" },
		"ceiling":         func(p *feedCursorPayload) { p.Ceiling = "bad" },
		"zero ceiling":    func(p *feedCursorPayload) { p.Ceiling = "0001-01-01T00:00:00Z" },
		"offset ceiling":  func(p *feedCursorPayload) { p.Ceiling = "2026-09-19T12:00:00+00:00" },
		"timestamp":       func(p *feedCursorPayload) { p.Timestamp = "bad" },
		"zero timestamp":  func(p *feedCursorPayload) { p.Timestamp = "0001-01-01T00:00:00Z" },
		"offset time":     func(p *feedCursorPayload) { p.Timestamp = "2026-09-19T11:00:00+00:00" },
		"fraction time":   func(p *feedCursorPayload) { p.Timestamp = "2026-09-19T11:00:00.000Z" },
		"after ceiling":   func(p *feedCursorPayload) { p.Timestamp = "2026-09-19T12:00:01Z" },
		"event kind":      func(p *feedCursorPayload) { p.Kind = "quote" },
		"id":              func(p *feedCursorPayload) { p.ID = "invalid" },
		"zero id":         func(p *feedCursorPayload) { p.ID = "00000000-0000-0000-0000-000000000000" },
		"uppercase id":    func(p *feedCursorPayload) { p.ID = app.ID(strings.ToUpper(string(p.ID))) },
		"negative score":  func(p *feedCursorPayload) { p.Sort = app.FeedSortReacted; p.Score = -1 },
		"newest score":    func(p *feedCursorPayload) { p.Score = 1 },
		"relevance score": func(p *feedCursorPayload) { p.Sort = app.FeedSortRelevant; p.Score = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			payload := valid
			change(&payload)
			contents, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			// Use the forged binding too: semantic validation must reject even a
			// correctly signed payload that matches its requested binding.
			if _, _, err := s.decodeFeedCursor(signRawTestCursor(s.cursorSigningKey, contents), payload.feedCursorBinding); !errors.Is(err, errInvalidCursor) {
				t.Fatalf("accepted %+v", payload)
			}
		})
	}
	contents, _ := json.Marshal(valid)
	for _, contents := range []string{
		string(contents) + " ", string(contents) + "{}", string(contents) + " trailing",
		strings.Replace(string(contents), `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(string(contents), `"version":1`, `"version":1,"unknown":1`, 1),
		strings.Replace(string(contents), `"version":1`, `"Version":1`, 1),
		strings.Replace(string(contents), `"score":0`, `"score":9223372036854775808`, 1),
		strings.Replace(string(contents), `"score":0`, `"score":0.0`, 1),
		strings.Replace(string(contents), `"score":0`, `"score":null`, 1),
		strings.Replace(string(contents), `"view":"for-you"`, `"view":"for\u002dyou"`, 1),
		strings.Replace(string(contents), `"viewer":"",`, "", 1),
	} {
		if _, _, err := s.decodeFeedCursor(signRawTestCursor(s.cursorSigningKey, []byte(contents)), valid.feedCursorBinding); !errors.Is(err, errInvalidCursor) {
			t.Fatalf("accepted noncanonical/overflow JSON %s", contents)
		}
	}
	token := signRawTestCursor(s.cursorSigningKey, contents)
	parts := strings.Split(token, ".")
	for _, token := range []string{
		"", "e30", "e30.", ".", "e30..x", "not-base64.sig", token + "x", "x" + token,
		parts[0] + "=." + parts[1], parts[0] + "." + parts[1] + "=",
		parts[0] + "\n." + parts[1], parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte("short")),
		strings.Repeat("x", maxCursorBytes+1),
	} {
		if _, _, err := s.decodeFeedCursor(token, valid.feedCursorBinding); !errors.Is(err, errInvalidCursor) {
			t.Fatalf("accepted malformed token %q", token)
		}
	}
	legacy, err := s.encodeCursor("replies", valid.ID, "", app.ReplySortOldest, time.Now(), &app.KeysetPosition{ID: valid.ID, Timestamp: time.Now().Add(-time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.decodeFeedCursor(*legacy, valid.feedCursorBinding); !errors.Is(err, errInvalidCursor) {
		t.Fatal("accepted reply cursor as feed cursor")
	}
	if _, _, err := s.decodeCursor(token, "replies", valid.ID, "", app.ReplySortOldest); !errors.Is(err, errInvalidCursor) {
		t.Fatal("accepted feed cursor as reply cursor")
	}
}
