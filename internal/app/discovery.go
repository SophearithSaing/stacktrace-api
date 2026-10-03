package app

import (
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultSearchLimit = 4
	MaxSearchLimit     = 20
)

// SearchPosition identifies a post in newest-first search ordering.
type SearchPosition struct {
	CreatedAt time.Time
	ID        ID
}

// SearchWindow bounds one search page and its original time ceiling.
type SearchWindow struct {
	Limit          int
	Position       *SearchPosition
	InitialCeiling time.Time
}

// SearchQuery describes a bounded public post search.
type SearchQuery struct {
	Text   string
	Window SearchWindow
}

// SearchItem combines a canonical post projection with a plain-text excerpt.
type SearchItem struct {
	Post    Post
	Snippet string
}

// SearchPage contains one page of matching posts.
type SearchPage struct {
	Items        []SearchItem
	NextPosition *SearchPosition
	Ceiling      time.Time
}

// Normalize validates a search term and pagination window.
func (query SearchQuery) Normalize() (SearchQuery, error) {
	query.Text = strings.TrimSpace(query.Text)
	if !utf8.ValidString(query.Text) || strings.ContainsRune(query.Text, 0) || utf8.RuneCountInString(query.Text) < 2 || utf8.RuneCountInString(query.Text) > 100 {
		return query, &ValidationError{Fields: map[string]string{"q": "Must be 2-100 Unicode code points"}}
	}
	if query.Window.Limit == 0 {
		query.Window.Limit = DefaultSearchLimit
	}
	if query.Window.Limit < 1 || query.Window.Limit > MaxSearchLimit {
		return query, &ValidationError{Fields: map[string]string{"limit": "Must be between 1 and 20"}}
	}
	if query.Window.Position != nil {
		position := *query.Window.Position
		id, err := ParseID(string(position.ID))
		if err != nil || position.CreatedAt.IsZero() || query.Window.InitialCeiling.IsZero() || position.CreatedAt.After(query.Window.InitialCeiling) {
			return query, &ValidationError{Fields: map[string]string{"cursor": "Must contain a valid search position"}}
		}
		position.ID = id
		position.CreatedAt = position.CreatedAt.UTC()
		query.Window.Position = &position
	}
	query.Window.InitialCeiling = query.Window.InitialCeiling.UTC()
	return query, nil
}

// SearchSnippet returns a Unicode-safe, bounded plain-text body excerpt.
func SearchSnippet(body string) string {
	runes := []rune(body)
	if len(runes) <= 160 {
		return body
	}
	return string(runes[:159]) + "…"
}
