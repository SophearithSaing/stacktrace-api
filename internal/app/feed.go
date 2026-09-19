package app

import (
	"strings"
	"time"
)

type FeedView string

const (
	FeedViewForYou    FeedView = "for-you"
	FeedViewFollowing FeedView = "following"
	FeedViewSpicy     FeedView = "spicy"
)

type FeedSort string

const (
	FeedSortNewest   FeedSort = "newest"
	FeedSortRelevant FeedSort = "relevant"
	FeedSortReacted  FeedSort = "reacted"
)

type FeedKind string

const (
	FeedKindPost   FeedKind = "post"
	FeedKindRepost FeedKind = "repost"
)

// FeedEntry identifies an event, not its canonical Post. ID can be shared by
// different kinds; the pair (Kind, ID) is the event's identity.
type FeedEntry struct {
	ID         ID
	Kind       FeedKind
	OccurredAt time.Time
	Reposter   *Account
	Post       Post
}

// FeedPosition is ordered by Score (reacted only), then Timestamp, Kind and ID,
// all descending. Relevant deliberately uses the newest ordering.
type FeedPosition struct {
	Score     int64
	Timestamp time.Time
	Kind      FeedKind
	ID        ID
}

type FeedPage struct {
	Items        []FeedEntry
	NextPosition *FeedPosition
	Ceiling      time.Time
}

// FeedWindow bounds event times, not changes to follows, deletions or reactions
// between requests. Reacted pagination is best effort when scores change.
type FeedWindow struct {
	Limit          int
	Position       *FeedPosition
	InitialCeiling time.Time
}

type FeedQuery struct {
	View   FeedView
	Sort   FeedSort
	Tag    string
	Window FeedWindow
}

// Normalize supplies defaults and validates the global feed selection. An empty
// tag means no filter; a provided tag must be one bare ASCII slug.
func (query FeedQuery) Normalize() (FeedQuery, error) {
	if query.View == "" {
		query.View = FeedViewForYou
	}
	if query.View != FeedViewForYou && query.View != FeedViewFollowing && query.View != FeedViewSpicy {
		return query, &ValidationError{Fields: map[string]string{"view": "Must be for-you, following or spicy"}}
	}
	if query.Sort == "" {
		query.Sort = FeedSortNewest
	}
	if len(query.Tag) > 64 {
		return query, &ValidationError{Fields: map[string]string{"tag": "Must be a bare ASCII slug of 1-64 letters, digits or underscores"}}
	}
	for i := range len(query.Tag) {
		if !isTagCharacter(query.Tag[i]) {
			return query, &ValidationError{Fields: map[string]string{"tag": "Must be a bare ASCII slug of 1-64 letters, digits or underscores"}}
		}
	}
	query.Tag = strings.ToLower(query.Tag)
	var err error
	query.Window, err = query.Window.Normalize(query.Sort)
	return query, err
}

// Normalize validates a window for the requested ordering. Account feeds pass
// FeedSortNewest; their public query has no view, sort or tag choices.
func (window FeedWindow) Normalize(sort FeedSort) (FeedWindow, error) {
	if sort != FeedSortNewest && sort != FeedSortRelevant && sort != FeedSortReacted {
		return window, &ValidationError{Fields: map[string]string{"sort": "Must be newest, relevant or reacted"}}
	}
	if window.Limit == 0 {
		window.Limit = DefaultReadLimit
	}
	if window.Limit < 1 || window.Limit > MaxReadLimit {
		return window, &ValidationError{Fields: map[string]string{"limit": "Must be between 1 and 50"}}
	}
	if window.Position != nil {
		position := *window.Position
		id, err := ParseID(string(position.ID))
		if err != nil {
			return window, err
		}
		position.ID = id
		if position.Kind != FeedKindPost && position.Kind != FeedKindRepost {
			return window, &ValidationError{Fields: map[string]string{"position": "Must include a valid event kind"}}
		}
		if position.Timestamp.IsZero() || window.InitialCeiling.IsZero() || position.Timestamp.After(window.InitialCeiling) {
			return window, &ValidationError{Fields: map[string]string{"position": "Must include an event timestamp at or before the required ceiling"}}
		}
		if position.Score < 0 || (sort != FeedSortReacted && position.Score != 0) {
			return window, &ValidationError{Fields: map[string]string{"position": "Score must be nonnegative and zero unless sorting by reacted"}}
		}
		position.Timestamp = position.Timestamp.UTC()
		window.Position = &position
	}
	window.InitialCeiling = window.InitialCeiling.UTC()
	return window, nil
}
