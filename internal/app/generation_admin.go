package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"
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

// DecodeAdminPersona turns one strict persona document into a Persona. The
// caller supplies the validated target agent UUID; files cannot redirect work to
// another account. Unknown, duplicate, missing, null, case-alias and trailing
// input all fail with safe, non-echoing errors. Domain validation rules reuse
// Persona.Validate so the trusted CLI and storage boundaries stay consistent.
func DecodeAdminPersona(data []byte, agentID ID) (Persona, error) {
	if len(data) > MaxAdminPersonaFileBytes {
		return Persona{}, errors.New("persona file exceeds size limit")
	}
	if !utf8.Valid(data) {
		return Persona{}, errors.New("invalid persona payload")
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
	persona := Persona{AgentID: agentID}
	if raw, ok := fields["version"]; ok {
		if err := json.Unmarshal(raw, &persona.Version); err != nil {
			return Persona{}, errors.New("invalid persona payload")
		}
	} else {
		return Persona{}, errors.New("invalid persona payload")
	}
	if raw, ok := fields["instructions"]; ok {
		if err := json.Unmarshal(raw, &persona.Instructions); err != nil {
			return Persona{}, errors.New("invalid persona payload")
		}
	} else {
		return Persona{}, errors.New("invalid persona payload")
	}
	if raw, ok := fields["topic_tags"]; ok {
		if err := json.Unmarshal(raw, &persona.TopicTags); err != nil {
			return Persona{}, errors.New("invalid persona payload")
		}
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
	if err := persona.Validate(); err != nil {
		return Persona{}, errors.New("invalid persona payload")
	}
	return persona, nil
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
