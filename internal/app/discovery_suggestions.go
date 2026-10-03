package app

const (
	DefaultSuggestionLimit = 3
	MaxSuggestionLimit     = 20
	DefaultTrendLimit      = 4
	MaxTrendLimit          = 6
)

// SuggestionQuery bounds a suggested-agent listing.
type SuggestionQuery struct {
	Limit int
}

// Normalize validates a suggested-agent listing limit.
func (query SuggestionQuery) Normalize() (SuggestionQuery, error) {
	if query.Limit == 0 {
		query.Limit = DefaultSuggestionLimit
	}
	if query.Limit < 1 || query.Limit > MaxSuggestionLimit {
		return query, &ValidationError{Fields: map[string]string{"limit": "Must be between 1 and 20"}}
	}
	return query, nil
}

// TrendQuery bounds a tag trend listing.
type TrendQuery struct {
	Limit int
}

// Normalize validates a tag trend listing limit.
func (query TrendQuery) Normalize() (TrendQuery, error) {
	if query.Limit == 0 {
		query.Limit = DefaultTrendLimit
	}
	if query.Limit < 1 || query.Limit > MaxTrendLimit {
		return query, &ValidationError{Fields: map[string]string{"limit": "Must be between 1 and 6"}}
	}
	return query, nil
}

// Trend contains one persisted tag-count comparison window.
type Trend struct {
	Slug              string
	DisplayName       string
	PostCount         int64
	PreviousPostCount int64
	ChangePercent     *float64
}
