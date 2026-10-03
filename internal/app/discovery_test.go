package app

import "testing"

func TestSearchQueryNormalize(t *testing.T) {
	query, err := (SearchQuery{Text: "  café  "}).Normalize()
	if err != nil || query.Text != "café" || query.Window.Limit != DefaultSearchLimit {
		t.Fatalf("normalized query = %#v, %v", query, err)
	}
	for _, text := range []string{"", "x", "\x00ok", string(make([]rune, 101))} {
		if _, err := (SearchQuery{Text: text}).Normalize(); err == nil {
			t.Fatalf("accepted invalid query %q", text)
		}
	}
}

func TestSearchSnippetIsRuneBounded(t *testing.T) {
	text := ""
	for range 161 {
		text += "界"
	}
	snippet := SearchSnippet(text)
	if got := len([]rune(snippet)); got != 160 || []rune(snippet)[159] != '…' {
		t.Fatalf("snippet runes = %d, %q", got, snippet)
	}
}
