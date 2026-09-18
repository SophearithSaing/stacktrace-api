package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxBodyRunes       = 320
	MaxCodeSourceBytes = 20 * 1024
	MaxTagsPerPost     = 5
)

var (
	ErrInvalidReactionKind   = errors.New("invalid reaction kind")
	ErrInvalidIdempotencyKey = errors.New("idempotency key must contain 1-128 ASCII letters, digits, periods, underscores, colons, or hyphens")
)

// Code is optional structured source attached to a post. Source is preserved
// exactly; language and filename are normalized metadata.
type Code struct {
	Language string
	Filename string
	Source   string
}

type Tag struct {
	Slug        string
	DisplayName string
}

// Content is normalized plain text, its extracted tags, and optional code.
type Content struct {
	Body string
	Tags []Tag
	Code *Code
}

// NewContent validates a body and optional complete code object.
func NewContent(body string, code *Code) (Content, error) {
	fields := map[string]string{}
	body, valid := normalizeBody(body)
	if !valid {
		fields["body"] = "Must contain 1-320 Unicode code points"
	}
	tags := ExtractTags(body)

	var normalizedCode *Code
	if code != nil {
		normalized := *code
		normalized.Language = strings.TrimSpace(normalized.Language)
		normalized.Filename = strings.TrimSpace(normalized.Filename)
		if !validCodeLanguage(normalized.Language) {
			fields["code.language"] = "Must contain 1-32 ASCII letters, digits, underscores, plus signs, periods, or hyphens"
		}
		if !validFilename(normalized.Filename) {
			fields["code.filename"] = "Must contain 1-255 Unicode code points without control characters"
		}
		if !utf8.ValidString(normalized.Source) || strings.ContainsRune(normalized.Source, 0) || len(normalized.Source) > MaxCodeSourceBytes || strings.TrimSpace(normalized.Source) == "" {
			fields["code.source"] = "Must be non-whitespace valid UTF-8 without NUL and at most 20 KiB"
		}
		normalizedCode = &normalized
	}
	if len(fields) != 0 {
		return Content{}, &ValidationError{Fields: fields}
	}
	return Content{Body: body, Tags: tags, Code: normalizedCode}, nil
}

func normalizeBody(body string) (string, bool) {
	body = strings.TrimSpace(body)
	return body, validText(body, 1, MaxBodyRunes)
}

func validText(value string, minimum, maximum int) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, 0) && utf8.RuneCountInString(value) >= minimum && utf8.RuneCountInString(value) <= maximum
}

func validCodeLanguage(value string) bool {
	if len(value) < 1 || len(value) > 32 {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '+' || character == '.' || character == '-') {
			return false
		}
	}
	return true
}

func validFilename(value string) bool {
	if !validText(value, 1, 255) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

// ExtractTags returns unique body hashtags in first-appearance order. An
// overlong candidate is consumed and ignored rather than truncated.
func ExtractTags(body string) []Tag {
	var tags []Tag
	seen := make(map[string]struct{})
	for index, character := range body {
		if character != '#' || (index > 0 && tagPrefixRune(body[:index])) {
			continue
		}
		end := index + 1
		for end < len(body) && isTagCharacter(body[end]) {
			end++
		}
		candidate := body[index+1 : end]
		if len(candidate) == 0 || len(candidate) > 64 {
			continue
		}
		slug := strings.ToLower(candidate)
		if _, exists := seen[slug]; exists {
			continue
		}
		seen[slug] = struct{}{}
		tags = append(tags, Tag{Slug: slug, DisplayName: candidate})
	}
	return tags
}

func tagPrefixRune(prefix string) bool {
	character, _ := utf8.DecodeLastRuneInString(prefix)
	return character == '#' || character == '_' || unicode.IsLetter(character) || unicode.IsDigit(character)
}

func isTagCharacter(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_'
}

type ReactionKind string

const (
	ReactionUseful    ReactionKind = "useful"
	ReactionAgree     ReactionKind = "agree"
	ReactionBrilliant ReactionKind = "brilliant"
	ReactionSpicy     ReactionKind = "spicy"
	ReactionShip      ReactionKind = "ship"
)

func (kind ReactionKind) Valid() bool {
	return kind == ReactionUseful || kind == ReactionAgree || kind == ReactionBrilliant || kind == ReactionSpicy || kind == ReactionShip
}

func ParseReactionKind(value string) (ReactionKind, error) {
	kind := ReactionKind(value)
	if !kind.Valid() {
		return "", ErrInvalidReactionKind
	}
	return kind, nil
}

type ReactionCounts struct {
	Useful    int64
	Agree     int64
	Brilliant int64
	Spicy     int64
	Ship      int64
}

type PostCounts struct {
	Replies        int64
	Reposts        int64
	ReactionsTotal int64
	Reactions      ReactionCounts
}

type PostViewer struct {
	Reaction     *ReactionKind
	Reposted     bool
	Bookmarked   bool
	ViewerRepost *Repost
}

// Repost identifies the durable repost event. It is separate from its source
// post so a remove-and-repost operation can receive a new event identity.
type Repost struct {
	ID        ID
	PostID    ID
	AccountID ID
	CreatedAt time.Time
}

type QuotePost struct {
	ID           ID
	Availability ContentAvailability
	Author       *Account
	Body         string
}

type ContentAvailability string

const (
	ContentAvailable ContentAvailability = "available"
	ContentDeleted   ContentAvailability = "deleted"
)

type Post struct {
	ID           ID
	Author       Account
	Content      Content
	Quote        *QuotePost
	IsSpicy      bool
	IsGenerated  bool
	CreatedAt    time.Time
	Counts       PostCounts
	Viewer       *PostViewer
	ReplyPreview ReplyPreview
}

type Reply struct {
	ID          ID
	PostID      ID
	Author      Account
	Body        string
	IsGenerated bool
	CreatedAt   time.Time
}

// CreateReplyResult includes the authoritative current total, which is not
// inferred from any embedded preview.
type CreateReplyResult struct {
	Reply      Reply
	ReplyTotal int64
}

type ReplyPreview struct {
	Items        []Reply
	NextPosition *KeysetPosition
	Ceiling      time.Time
}

// ReplyPage is a bounded keyset page and the authoritative current visible
// reply total for its parent post.
type ReplyPage struct {
	Items        []Reply
	NextPosition *KeysetPosition
	Ceiling      time.Time
	ReplyTotal   int64
}

// PostPage is a bounded keyset page of canonical post projections.
type PostPage struct {
	Items        []Post
	NextPosition *KeysetPosition
	Ceiling      time.Time
}

type ReplySort string

const (
	ReplySortOldest ReplySort = "oldest"
	ReplySortNewest ReplySort = "newest"
)

type KeysetPosition struct {
	Timestamp time.Time
	ID        ID
}

type ReadWindow struct {
	Sort           ReplySort
	Limit          int
	Position       *KeysetPosition
	InitialCeiling time.Time
}

const (
	DefaultReadLimit = 20
	MaxReadLimit     = 50
)

// PostCreation and ReplyCreation are normalized creation operations used for
// persistence and idempotency hashing.
type PostCreation struct {
	Content      Content
	QuotedPostID *ID
}

func NewPostCreation(body string, quotedPostID *ID, code *Code) (PostCreation, error) {
	content, err := NewContent(body, code)
	if err != nil {
		return PostCreation{}, err
	}
	if len(content.Tags) > MaxTagsPerPost {
		return PostCreation{}, &ValidationError{Fields: map[string]string{"body": "Must contain at most 5 unique hashtags"}}
	}
	var normalizedQuoteID *ID
	if quotedPostID != nil {
		parsedID, err := ParseID(string(*quotedPostID))
		if err != nil {
			return PostCreation{}, err
		}
		normalizedQuoteID = &parsedID
	}
	return PostCreation{Content: content, QuotedPostID: normalizedQuoteID}, nil
}

type ReplyCreation struct {
	PostID ID
	Body   string
}

func NewReplyCreation(postID ID, body string) (ReplyCreation, error) {
	parsedID, err := ParseID(string(postID))
	if err != nil {
		return ReplyCreation{}, err
	}
	body, valid := normalizeBody(body)
	if !valid {
		return ReplyCreation{}, &ValidationError{Fields: map[string]string{"body": "Must contain 1-320 Unicode code points"}}
	}
	return ReplyCreation{PostID: parsedID, Body: body}, nil
}

func ValidateIdempotencyKey(key string) error {
	if len(key) < 1 || len(key) > 128 {
		return ErrInvalidIdempotencyKey
	}
	for _, character := range key {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '.' || character == '_' || character == ':' || character == '-') {
			return ErrInvalidIdempotencyKey
		}
	}
	return nil
}

func (creation PostCreation) RequestHash() string {
	return hashCreation("POST /posts", creation.Content, creation.QuotedPostID, nil)
}

func (creation ReplyCreation) RequestHash() string {
	return hashCreation("POST /posts/{postID}/replies", Content{Body: creation.Body}, nil, &creation.PostID)
}

func hashCreation(operation string, content Content, quoteID, replyPostID *ID) string {
	hash := sha256.New()
	writeHashField(hash, "stacktrace-content-v1")
	writeHashField(hash, operation)
	writeHashField(hash, content.Body)
	writeOptionalID(hash, quoteID)
	writeOptionalID(hash, replyPostID)
	if content.Code == nil {
		writeHashField(hash, "")
	} else {
		writeHashField(hash, "code")
		writeHashField(hash, content.Code.Language)
		writeHashField(hash, content.Code.Filename)
		writeHashField(hash, content.Code.Source)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func writeOptionalID(hash interface{ Write([]byte) (int, error) }, id *ID) {
	if id == nil {
		writeHashField(hash, "")
		return
	}
	writeHashField(hash, "id")
	writeHashField(hash, string(*id))
}

func writeHashField(hash interface{ Write([]byte) (int, error) }, value string) {
	hash.Write([]byte{byte(len(value) >> 24), byte(len(value) >> 16), byte(len(value) >> 8), byte(len(value))})
	hash.Write([]byte(value))
}
