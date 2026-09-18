package app

import (
	"errors"
	"strings"
	"testing"
)

func TestNewContentBodyValidation(t *testing.T) {
	validCombining := strings.Repeat("e\u0301", 160)
	tests := []struct {
		name string
		body string
		ok   bool
	}{
		{"trims whitespace", "  hello  ", true},
		{"empty", "", false},
		{"Unicode whitespace", "\u2003\u2009", false},
		{"320 emoji", strings.Repeat("😀", 320), true},
		{"321 emoji", strings.Repeat("😀", 321), false},
		{"320 combining code points", validCombining, true},
		{"321 combining code points", validCombining + "e", false},
		{"invalid UTF-8", string([]byte{0xff}), false},
		{"NUL", "hello\x00", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content, err := NewContent(test.body, nil)
			if (err == nil) != test.ok {
				t.Fatalf("NewContent() error = %v", err)
			}
			if test.name == "trims whitespace" && content.Body != "hello" {
				t.Fatalf("body = %q", content.Body)
			}
		})
	}
}

func TestNewContentCodeValidation(t *testing.T) {
	exactSource := strings.Repeat("é", MaxCodeSourceBytes/2)
	tests := []struct {
		name string
		code Code
		ok   bool
	}{
		{"valid normalized metadata", Code{" go ", " main.go ", "  package main\n"}, true},
		{"empty language", Code{"", "x.go", "x"}, false},
		{"empty filename", Code{"go", "", "x"}, false},
		{"empty source", Code{"go", "x.go", ""}, false},
		{"whitespace source", Code{"go", "x.go", "\u2003"}, false},
		{"invalid UTF-8 source", Code{"go", "x.go", string([]byte{0xff})}, false},
		{"NUL source", Code{"go", "x.go", "x\x00y"}, false},
		{"language 32", Code{strings.Repeat("a", 32), "x", "x"}, true},
		{"language 33", Code{strings.Repeat("a", 33), "x", "x"}, false},
		{"illegal language", Code{"go lang", "x", "x"}, false},
		{"filename 255", Code{"go", strings.Repeat("é", 255), "x"}, true},
		{"filename 256", Code{"go", strings.Repeat("é", 256), "x"}, false},
		{"control filename", Code{"go", "x\ny", "x"}, false},
		{"invalid UTF-8 filename", Code{"go", string([]byte{0xff}), "x"}, false},
		{"exact 20 KiB UTF-8 source", Code{"go", "x", exactSource}, true},
		{"over 20 KiB multibyte source", Code{"go", "x", exactSource + "é"}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content, err := NewContent("post", &test.code)
			if (err == nil) != test.ok {
				t.Fatalf("NewContent() error = %v", err)
			}
			if test.name == "valid normalized metadata" && (content.Code.Language != "go" || content.Code.Filename != "main.go" || content.Code.Source != test.code.Source) {
				t.Fatalf("code = %#v", content.Code)
			}
		})
	}
}

func TestExtractTags(t *testing.T) {
	tag64 := strings.Repeat("a", 64)
	tag65 := strings.Repeat("b", 65)
	tags := ExtractTags("#Go #go x#no é#nein _#also ##never #" + tag64 + " #" + tag65)
	if len(tags) != 2 {
		t.Fatalf("tag count = %d, tags = %#v", len(tags), tags)
	}
	if tags[0] != (Tag{Slug: "go", DisplayName: "Go"}) || tags[1] != (Tag{Slug: tag64, DisplayName: tag64}) {
		t.Fatalf("tags = %#v", tags)
	}
}

func TestPostAndReplyTagLimits(t *testing.T) {
	fiveTags := "#a #b #c #d #e"
	sixTags := fiveTags + " #f"
	if _, err := NewPostCreation(fiveTags, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPostCreation(sixTags, nil, nil); err == nil {
		t.Fatal("post with six tags accepted")
	}
	postID := ID("11111111-1111-1111-1111-111111111111")
	if _, err := NewReplyCreation(postID, sixTags); err != nil {
		t.Fatalf("reply with six tags rejected: %v", err)
	}
}

func TestReactionKind(t *testing.T) {
	for _, kind := range []ReactionKind{ReactionUseful, ReactionAgree, ReactionBrilliant, ReactionSpicy, ReactionShip} {
		if !kind.Valid() {
			t.Fatalf("%q invalid", kind)
		}
	}
	if _, err := ParseReactionKind("like"); err == nil {
		t.Fatal("invalid kind accepted")
	}
}

func TestIdempotencyKeyValidation(t *testing.T) {
	for _, key := range []string{"a", strings.Repeat("a", 128)} {
		if err := ValidateIdempotencyKey(key); err != nil {
			t.Fatalf("valid key %q: %v", key, err)
		}
	}
	for _, key := range []string{"", strings.Repeat("a", 129), "has space", "é", "slash/key"} {
		if err := ValidateIdempotencyKey(key); err == nil {
			t.Fatalf("invalid key accepted: %q", key)
		}
	}
}

func TestCreationIDsAndRequestHash(t *testing.T) {
	uppercaseQuote := ID("ABCDEFAB-CDEF-CDEF-CDEF-ABCDEFABCDEF")
	lowercaseQuote := ID(strings.ToLower(string(uppercaseQuote)))
	first, err := NewPostCreation(" hello ", &uppercaseQuote, &Code{Language: " go ", Filename: " x.go ", Source: "x\n"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewPostCreation("hello", &lowercaseQuote, &Code{Language: "go", Filename: "x.go", Source: "x\n"})
	if err != nil {
		t.Fatal(err)
	}
	if first.RequestHash() != second.RequestHash() {
		t.Fatal("equivalent normalized posts hashed differently")
	}
	originalHash := first.RequestHash()
	uppercaseQuote = ID("11111111-1111-1111-1111-111111111111")
	if first.RequestHash() != originalHash {
		t.Fatal("post hash changed after quote pointer mutation")
	}

	changes := []struct {
		name string
		post PostCreation
	}{
		{"body", mustPostCreation(t, "other", &lowercaseQuote, &Code{"go", "x.go", "x\n"})},
		{"quote", mustPostCreation(t, "hello", idPointer("11111111-1111-1111-1111-111111111111"), &Code{"go", "x.go", "x\n"})},
		{"code absent", mustPostCreation(t, "hello", &lowercaseQuote, nil)},
		{"code language", mustPostCreation(t, "hello", &lowercaseQuote, &Code{"rust", "x.go", "x\n"})},
		{"code filename", mustPostCreation(t, "hello", &lowercaseQuote, &Code{"go", "y.go", "x\n"})},
		{"code source", mustPostCreation(t, "hello", &lowercaseQuote, &Code{"go", "x.go", "y\n"})},
	}
	for _, change := range changes {
		if originalHash == change.post.RequestHash() {
			t.Fatalf("hash did not change for %s", change.name)
		}
	}

	firstReplyTarget := ID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	secondReplyTarget := ID("11111111-1111-1111-1111-111111111111")
	replyOne, err := NewReplyCreation(firstReplyTarget, "hello")
	if err != nil {
		t.Fatal(err)
	}
	replyTwo, err := NewReplyCreation(secondReplyTarget, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if replyOne.RequestHash() == replyTwo.RequestHash() {
		t.Fatal("reply target did not affect hash")
	}
	if first.RequestHash() == replyOne.RequestHash() {
		t.Fatal("post and reply operations share hash")
	}

	if _, err := NewPostCreation("post", idPointer("bad"), nil); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("invalid quote error = %v", err)
	}
	if _, err := NewReplyCreation(ID("bad"), "reply"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("invalid reply target error = %v", err)
	}
}

func mustPostCreation(t *testing.T, body string, quote *ID, code *Code) PostCreation {
	t.Helper()
	creation, err := NewPostCreation(body, quote, code)
	if err != nil {
		t.Fatal(err)
	}
	return creation
}

func idPointer(value string) *ID {
	id := ID(value)
	return &id
}
