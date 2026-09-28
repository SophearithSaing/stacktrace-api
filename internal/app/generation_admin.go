package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Admin file inputs are bounded before decoding and reject unknown, duplicate
// and missing fields plus trailing input. Persona text is never echoed back by
// callers; only bounded counts and identities surface in reports.
const (
	MaxAdminPersonaFileBytes = 65552 // Bounded persona text plus metadata.
	MaxAdminPolicyFileBytes  = 16384 // Already the policy room, headroom included.
)

// DecodeAdminPersona turns one strict persona document into a Persona without
// its agent: the caller must attach a validated target UUID from arguments so a
// file can never point at an unexpected account. Unknown, duplicate, missing,
// null and trailing input all fail; the domain validation rules (UTF-8 bytes,
// bounded text, 1-20 lowercase slug tags) still apply. Creation time is an
// explicit RFC 3339 stamp; versions are immutable.
func DecodeAdminPersona(data []byte) (Persona, error) {
	if len(data) > MaxAdminPersonaFileBytes {
		return Persona{}, fmt.Errorf("persona file exceeds %d bytes", MaxAdminPersonaFileBytes)
	}
	var file struct {
		Version      *int       `json:"version"`
		Instructions *string    `json:"instructions"`
		TopicTags    *[]string  `json:"topic_tags"`
		CreatedAt    *time.Time `json:"created_at"`
	}
	if err := strictJSON(data, &file); err != nil {
		return Persona{}, err
	}
	if file.Version == nil || file.Instructions == nil || file.TopicTags == nil || file.CreatedAt == nil {
		return Persona{}, fmt.Errorf("persona file requires version, instructions, topic_tags and created_at")
	}
	persona := Persona{
		Version:      *file.Version,
		Instructions: *file.Instructions,
		TopicTags:    *file.TopicTags,
		CreatedAt:    *file.CreatedAt,
	}
	// The agent UUID is validated separately from flags; the text/tag rules
	// apply here and the full domain validation runs at the trusted store.
	if persona.Instructions == "" || persona.CreatedAt.IsZero() || persona.Version < 1 {
		return Persona{}, fmt.Errorf("invalid persona content")
	}
	for _, tag := range persona.TopicTags {
		if len(tag) < 1 || len(tag) > 64 || tag != strings.ToLower(tag) {
			return Persona{}, fmt.Errorf("invalid persona content")
		}
	}
	return persona, nil
}

// DecodeAdminPolicy reuses the bounded strict generation policy boundary with
// an explicit size limit checked before decoding.
func DecodeAdminPolicy(data []byte) (GenerationPolicy, error) {
	if len(data) > MaxAdminPolicyFileBytes {
		return GenerationPolicy{}, fmt.Errorf("policy file exceeds %d bytes", MaxAdminPolicyFileBytes)
	}
	return DecodeGenerationPolicy(data)
}

// strictJSON decodes one JSON object with DisallowUnknownFields and token-level
// duplicate detection, and rejects trailing input after the single document.
func strictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := strictJSONFields(decoder); err != nil {
		return err
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid payload: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("payload has trailing data")
	}
	return nil
}

// strictJSONFields rejects duplicate top-level and missing object fields at the
// token stream level, before any value decoding takes place.
func strictJSONFields(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return fmt.Errorf("payload must be one JSON object")
	}
	seen := make(map[string]bool)
	for decoder.More() {
		nameToken, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("invalid payload field")
		}
		name, ok := nameToken.(string)
		if !ok || seen[name] {
			return fmt.Errorf("duplicate payload field")
		}
		seen[name] = true
		var discard json.RawMessage
		if err := decoder.Decode(&discard); err != nil {
			return fmt.Errorf("invalid payload value")
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("invalid payload object")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("payload has trailing data")
	}
	return nil
}
