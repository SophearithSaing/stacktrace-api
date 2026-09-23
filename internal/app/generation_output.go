package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxGenerationOutputBytes      = 32 * 1024
	GenerationOutputDigestVersion = "generation_output_v1"
)

var ErrGenerationOutput = errors.New("invalid generation output")

type GenerationDecision string

const (
	GenerationPublish GenerationDecision = "publish"
	GenerationSkip    GenerationDecision = "skip"
)

// GenerationResult contains no model-selected identity, target or authority.
// Private fields keep the decoded, normalized value intact. Publication must
// still revalidate against the locked job, attempt digest and recent content.
type GenerationResult struct {
	decision GenerationDecision
	kind     GenerationOutput
	content  Content
	reason   string
}

func (r GenerationResult) Decision() GenerationDecision { return r.decision }
func (r GenerationResult) Reason() string               { return r.reason }
func (r GenerationResult) OutputKind() GenerationOutput { return r.kind }
func (r GenerationResult) Content() Content {
	content := r.content
	content.Tags = append([]Tag(nil), content.Tags...)
	if content.Code != nil {
		code := *content.Code
		content.Code = &code
	}
	return content
}

// DecodeGenerationResult accepts exactly {decision,body[,code]} or
// {decision,reason}. Even null optional keys in the wrong branch are rejected.
// The adapter must separately reject truncation, tools and unsupported reasoning.
func DecodeGenerationResult(data []byte, job GenerationJob) (GenerationResult, error) {
	if len(data) > MaxGenerationOutputBytes || !utf8.Valid(data) || job.Validate() != nil {
		return GenerationResult{}, ErrGenerationOutput
	}
	fields, err := generationObject(data, "decision", "body", "code", "reason")
	if err != nil {
		return GenerationResult{}, err
	}
	decision, err := generationString(fields["decision"])
	if err != nil {
		return GenerationResult{}, err
	}
	r := GenerationResult{decision: GenerationDecision(decision), kind: job.OutputKind}
	switch r.decision {
	case GenerationSkip:
		if len(fields) != 2 {
			return GenerationResult{}, ErrGenerationOutput
		}
		r.reason, err = generationString(fields["reason"])
		if err != nil || !validGenerationSkipReason(r.reason) {
			return GenerationResult{}, ErrGenerationOutput
		}
	case GenerationPublish:
		if _, exists := fields["reason"]; exists {
			return GenerationResult{}, ErrGenerationOutput
		}
		body, err := generationString(fields["body"])
		if err != nil {
			return GenerationResult{}, err
		}
		var code *Code
		if raw, exists := fields["code"]; exists {
			if job.OutputKind == OutputReply {
				return GenerationResult{}, ErrGenerationOutput
			}
			codeFields, err := generationObject(raw, "language", "filename", "source")
			if err != nil || len(codeFields) != 3 {
				return GenerationResult{}, ErrGenerationOutput
			}
			code = &Code{}
			for name, value := range map[string]*string{"language": &code.Language, "filename": &code.Filename, "source": &code.Source} {
				*value, err = generationString(codeFields[name])
				if err != nil {
					return GenerationResult{}, err
				}
			}
		}
		if job.OutputKind == OutputReply {
			creation, err := NewReplyCreation(*job.SourcePostID, body)
			if err != nil {
				return GenerationResult{}, ErrGenerationOutput
			}
			r.content = Content{Body: creation.Body}
		} else {
			var quote *ID
			if job.OutputKind == OutputQuote {
				quote = job.SourcePostID
			}
			creation, err := NewPostCreation(body, quote, code)
			if err != nil {
				return GenerationResult{}, ErrGenerationOutput
			}
			r.content = creation.Content
		}
	default:
		return GenerationResult{}, ErrGenerationOutput
	}
	return r, nil
}

func validGenerationSkipReason(reason string) bool {
	return reason == "not_relevant" || reason == "insufficient_context" || reason == "unsafe_request" || reason == "repetition"
}

// Digest binds the exact normalized decision, output kind, body and complete
// code attachment (or allowlisted skip reason). Tags are derived, not model input.
// Length framing avoids concatenation ambiguity. No raw output is retained.
func (r GenerationResult) Digest() string {
	if r.decision != GenerationPublish && r.decision != GenerationSkip {
		return ""
	}
	hash := sha256.New()
	for _, field := range []string{GenerationOutputDigestVersion, string(r.decision), string(r.kind), r.reason, r.content.Body} {
		writeHashField(hash, field)
	}
	if r.content.Code == nil {
		writeHashField(hash, "")
	} else {
		writeHashField(hash, "code")
		writeHashField(hash, r.content.Code.Language)
		writeHashField(hash, r.content.Code.Filename)
		writeHashField(hash, r.content.Code.Source)
	}
	return GenerationOutputDigestVersion + ":" + hex.EncodeToString(hash.Sum(nil))
}

func validGenerationOutputDigest(digest string) bool {
	hexValue, ok := strings.CutPrefix(digest, GenerationOutputDigestVersion+":")
	decoded, err := hex.DecodeString(hexValue)
	return ok && err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == hexValue
}

// ValidateGenerationPublicationOutput is a pure, fail-closed binding check, not
// publication authority. The store must additionally lock and recheck source,
// account/settings/policy/lease and perform all writes atomically. Historical
// succeeded attempts without a digest remain readable, but cannot authorize new
// publication. Re-run safety/repetition against a fresh bounded public snapshot.
func ValidateGenerationPublicationOutput(job GenerationJob, attempt GenerationAttempt, result GenerationResult, recent []Content, now time.Time) error {
	if job.Validate() != nil || job.ValidateLease(attempt.LeaseVersion, now) != nil || attempt.Validate() != nil || attempt.JobID != job.ID || attempt.Status != AttemptSucceeded || attempt.StartedAt.Before(job.AvailableAt) || attempt.FinishedAt.After(now) || result.Decision() != GenerationPublish || result.OutputKind() != job.OutputKind || attempt.OutputDigest == "" || result.Digest() != attempt.OutputDigest {
		return ErrGenerationOutput
	}
	return ValidateGenerationSafety(result, recent)
}

func generationObject(data []byte, allowed ...string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrGenerationOutput
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return nil, ErrGenerationOutput
		}
		accepted := false
		for _, field := range allowed {
			if name == field {
				accepted = true
			}
		}
		if !accepted || fields[name] != nil {
			return nil, ErrGenerationOutput
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, ErrGenerationOutput
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, ErrGenerationOutput
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, ErrGenerationOutput
	}
	return fields, nil
}

func generationString(data []byte) (string, error) {
	var value string
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) || json.Unmarshal(data, &value) != nil || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\ufffd") {
		return "", ErrGenerationOutput
	}
	// Reject replacement characters too: encoding/json repairs unpaired UTF-16
	// surrogates rather than reporting an error. Never publish repaired output.
	return value, nil
}
