package app

import (
	"strings"
	"testing"
	"time"
)

func TestFeedQueryNormalize(t *testing.T) {
	query, err := (FeedQuery{Tag: "Go_123"}).Normalize()
	if err != nil || query.View != FeedViewForYou || query.Sort != FeedSortNewest || query.Tag != "go_123" || query.Window.Limit != 20 {
		t.Fatalf("query=%+v err=%v", query, err)
	}
	for _, tag := range []string{"#go", " go", "go ", "go,rust", "go-rust", "é", strings.Repeat("a", 65)} {
		if _, err := (FeedQuery{Tag: tag}).Normalize(); err == nil {
			t.Errorf("accepted tag %q", tag)
		}
	}
	for _, tag := range []string{"", "_", "0", strings.Repeat("A", 64)} {
		if _, err := (FeedQuery{Tag: tag}).Normalize(); err != nil {
			t.Errorf("tag %q: %v", tag, err)
		}
	}
	for _, query := range []FeedQuery{{View: "unknown"}, {Sort: "oldest"}, {Window: FeedWindow{Limit: -1}}, {Window: FeedWindow{Limit: 51}}} {
		if _, err := query.Normalize(); err == nil {
			t.Errorf("accepted %+v", query)
		}
	}
	for _, view := range []FeedView{FeedViewForYou, FeedViewFollowing, FeedViewSpicy} {
		for _, sort := range []FeedSort{FeedSortNewest, FeedSortRelevant, FeedSortReacted} {
			if _, err := (FeedQuery{View: view, Sort: sort, Window: FeedWindow{Limit: 50}}).Normalize(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestFeedPositionValidation(t *testing.T) {
	now := time.Now().UTC()
	valid := FeedPosition{Timestamp: now, Kind: FeedKindRepost, ID: NewID()}
	for _, sort := range []FeedSort{FeedSortNewest, FeedSortRelevant, FeedSortReacted} {
		window, err := (FeedWindow{Position: &valid, InitialCeiling: now}).Normalize(sort)
		if err != nil || *window.Position != valid || window.Position == &valid {
			t.Fatalf("window=%+v error=%v", window, err)
		}
	}
	for _, change := range []func(*FeedPosition){
		func(p *FeedPosition) { p.ID = "invalid" },
		func(p *FeedPosition) { p.Kind = "unknown" },
		func(p *FeedPosition) { p.Timestamp = time.Time{} },
		func(p *FeedPosition) { p.Timestamp = now.Add(time.Second) },
		func(p *FeedPosition) { p.Score = -1 },
	} {
		position := valid
		change(&position)
		if _, err := (FeedWindow{Position: &position, InitialCeiling: now}).Normalize(FeedSortReacted); err == nil {
			t.Errorf("accepted %+v", position)
		}
	}
	if _, err := (FeedWindow{Position: &valid}).Normalize(FeedSortNewest); err == nil {
		t.Fatal("accepted position without ceiling")
	}
	valid.Score = 5
	for _, sort := range []FeedSort{FeedSortNewest, FeedSortRelevant} {
		if _, err := (FeedWindow{Position: &valid, InitialCeiling: now}).Normalize(sort); err == nil {
			t.Fatal("accepted score outside reacted")
		}
	}
	if _, err := (FeedWindow{Position: &valid, InitialCeiling: now}).Normalize(FeedSortReacted); err != nil {
		t.Fatal(err)
	}
}
