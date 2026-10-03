package app

import (
	"strings"
	"testing"
)

func TestSearchQueryNormalize(t *testing.T) {
	query, err := (SearchQuery{Text: "  café  "}).Normalize()
	if err != nil || query.Text != "café" || query.Window.Limit != DefaultSearchLimit {
		t.Fatalf("normalized query = %#v, %v", query, err)
	}
	for _, text := range []string{"", "x", "\x00ok", strings.Repeat("界", 101)} {
		if _, err := (SearchQuery{Text: text}).Normalize(); err == nil {
			t.Fatalf("accepted invalid query %q", text)
		}
	}
	if _, err := (SearchQuery{Text: strings.Repeat("界", 100), Window: SearchWindow{Limit: MaxSearchLimit}}).Normalize(); err != nil {
		t.Fatalf("rejected 100-code-point query: %v", err)
	}
	if _, err := (SearchQuery{Text: "valid", Window: SearchWindow{Limit: MaxSearchLimit + 1}}).Normalize(); err == nil {
		t.Fatal("accepted excessive limit")
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
	if got := SearchSnippet("plain text"); got != "plain text" {
		t.Fatalf("snippet changed short body %q", got)
	}
}
