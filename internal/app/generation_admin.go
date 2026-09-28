package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
)

// Admin file inputs are bounded before decoding and reject unknown, duplicate
// and missing fields plus trailing input. Persona text is never echoed back by
// callers; only bounded counts and identities surface in reports.
const (
	// MaxAdminPersonaFileBytes bounds the strict persona JSON document.
	// The largest allowed persona has 16,000 instruction bytes plus up to 20
	// topic tags, each up to 64 bytes; the cap leaves headroom for JSON
	// escaping and object framing while still rejecting pathological uploads.
	MaxAdminPersonaFileBytes = 65552
	// MaxAdminPolicyFileBytes matches the checked policy size used by the
	// configuration reader so oversized policy documents are rejected before
	// they reach the strict JSON boundary.
	MaxAdminPolicyFileBytes = 8192
)

// allowedPersonaFields lists the exact, case-sensitive keys permitted in a
// persona file. Versions are immutable and creation time is an explicit
// RFC 3339 stamp supplied by the caller.
var allowedPersonaFields = map[string]bool{
	"version":      true,
	"instructions": true,
	"topic_tags":   true,
	"created_at":   true,
}

// DecodeAdminPersona turns one strict persona document into a Persona without
// its agent: the caller must attach a validated target UUID from arguments so a
// file can never point at an unexpected account. Unknown, duplicate, missing,
// null, case-alias and trailing input all fail with safe, non-echoing errors.
// Domain validation rules (UTF-8 bytes, bounded text, 1-20 lowercase slug tags)
// still apply and are checked explicitly here because the agent UUID is only
// validated separately at the trusted store boundary.
func DecodeAdminPersona(data []byte) (Persona, error) {
	if len(data) > MaxAdminPersonaFileBytes {
		return Persona{}, errors.New("persona file exceeds size limit")
	}
	fields, err := parseStrictJSONObject(data)
	if err != nil {
		return Persona{}, errors.New("invalid persona payload")
	}
	for name := range fields {
		if !allowedPersonaFields[name] {
			return Persona{}, errors.New("invalid persona payload")
		}
	}
	var persona Persona
	if raw, ok := fields["version"]; ok {
		num, err := jsonNumberInt(raw)
		if err != nil || num < 1 {
			return Persona{}, errors.New("invalid persona payload")
		}
		persona.Version = num
	} else {
		return Persona{}, errors.New("invalid persona payload")
	}
	if raw, ok := fields["instructions"]; ok {
		var instructions string
		if err := json.Unmarshal(raw, &instructions); err != nil {
			return Persona{}, errors.New("invalid persona payload")
		}
		persona.Instructions = instructions
	} else {
		return Persona{}, errors.New("invalid persona payload")
	}
	if raw, ok := fields["topic_tags"]; ok {
		var tags []string
		if err := json.Unmarshal(raw, &tags); err != nil {
			return Persona{}, errors.New("invalid persona payload")
		}
		persona.TopicTags = tags
	} else {
		return Persona{}, errors.New("invalid persona payload")
	}
	if raw, ok := fields["created_at"]; ok {
		var created string
		if err := json.Unmarshal(raw, &created); err != nil {
			return Persona{}, errors.New("invalid persona payload")
		}
		stamp, err := time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return Persona{}, errors.New("invalid persona payload")
		}
		persona.CreatedAt = stamp
	} else {
		return Persona{}, errors.New("invalid persona payload")
	}
	if err := validateAdminPersona(persona); err != nil {
		return Persona{}, errors.New("invalid persona payload")
	}
	return persona, nil
}

func validateAdminPersona(p Persona) error {
	if p.Version < 1 || p.Version > 2147483647 || p.CreatedAt.IsZero() {
		return fmt.Errorf("invalid persona identity or creation time")
	}
	if !validGenerationText(p.Instructions, 16000) {
		return fmt.Errorf("persona instructions must contain 1-16000 UTF-8 bytes without NUL")
	}
	if len(p.TopicTags) < 1 || len(p.TopicTags) > 20 {
		return fmt.Errorf("persona must have 1-20 topic tags")
	}
	seen := make(map[string]bool)
	for _, tag := range p.TopicTags {
		if len(tag) < 1 || len(tag) > 64 || tag != lowerASCII(tag) || seen[tag] {
			return fmt.Errorf("persona tags must be unique lowercase ASCII slugs of 1-64 bytes")
		}
		for i := range len(tag) {
			if !isTagCharacter(tag[i]) {
				return fmt.Errorf("persona tags must contain only ASCII letters, digits or underscores")
			}
		}
		seen[tag] = true
	}
	return nil
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// DecodeAdminPolicy reuses the bounded strict generation policy boundary with
// an explicit size limit checked before decoding. All decode and validation
// errors are sanitized at this trusted input boundary so user content never
// reaches stderr.
func DecodeAdminPolicy(data []byte) (GenerationPolicy, error) {
	if len(data) > MaxAdminPolicyFileBytes {
		return GenerationPolicy{}, errors.New("policy file exceeds size limit")
	}
	policy, err := DecodeGenerationPolicy(data)
	if err != nil {
		return GenerationPolicy{}, errors.New("invalid policy payload")
	}
	return policy, nil
}

// parseStrictJSONObject decodes one JSON object, rejecting non-objects,
// duplicate keys, trailing data and null field values. Keys are matched exactly
// as they appear in the source text so case-alias attacks are not possible.
func parseStrictJSONObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		return nil, fmt.Errorf("payload must be one JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		nameToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := nameToken.(string)
		if !ok {
			return nil, fmt.Errorf("invalid object key")
		}
		if _, exists := fields[name]; exists {
			return nil, fmt.Errorf("duplicate object key")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("null object value")
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("payload has trailing data")
	}
	return fields, nil
}

// jsonNumberInt parses a JSON number as a positive 32-bit integer.
func jsonNumberInt(raw json.RawMessage) (int, error) {
	var num json.Number
	if err := json.Unmarshal(raw, &num); err != nil {
		return 0, err
	}
	value, err := strconv.ParseInt(string(num), 10, 31)
	if err != nil {
		return 0, err
	}
	return int(value), nil
}
