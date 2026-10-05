package llm

import (
	"bytes"
	"encoding/json"
	"io"
)

// DecodeUsage accepts standard counters and the cached prompt-token subset,
// rejecting duplicate, unknown and case-alias fields. Missing
// or null counts are merely unknown, never zero. No untrusted data is returned
// in an error or diagnostic. The entire provider response must also be bounded.
func DecodeUsage(data []byte) (usage *Usage, unsupported bool) {
	usage, rejection := decodeUsage(data)
	return usage, rejection != ""
}

// decodeUsage returns a fixed private reason for unsupported usage shapes,
// without returning provider-controlled field names or scalar text as errors.
func decodeUsage(data []byte) (*Usage, string) {
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil, ""
	}
	if len(data) > 1024 {
		return nil, "usage_oversized"
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, "usage_not_object"
	}
	result := &Usage{}
	fields := map[string]**int64{"prompt_tokens": &result.PromptTokens, "completion_tokens": &result.CompletionTokens, "total_tokens": &result.TotalTokens, "cached_tokens": &result.CachedTokens}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return nil, "usage_field_invalid"
		}
		if seen[name] {
			return nil, "usage_duplicate_field"
		}
		if fields[name] == nil {
			return nil, "usage_unknown_field"
		}
		seen[name] = true
		if decoder.Decode(fields[name]) != nil {
			return nil, "usage_count_type_invalid"
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, "usage_object_malformed"
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, "usage_trailing_data"
	}
	if result.CachedTokens != nil && (*result.CachedTokens < 0 || result.PromptTokens == nil || *result.CachedTokens > *result.PromptTokens) {
		return nil, "usage_cached_tokens_invalid"
	}
	return result, ""
}

type UsageAssessment struct {
	AccountedTokens int64
	InputTokens     *int64
	OutputTokens    *int64
	StopExecution   bool
}

// AssessUsage separates operational targets from the financial reservation.
// Known usage within 8192/1024 may settle downward, irrespective of the local
// reference estimate. Unknown/negative/inconsistent usage retains all 132096.
// Exceeding either local target cannot borrow the other's slack or the much
// larger financial ceiling: retain the full reservation and stop for review.
// Unsupported accounting or uncertain remote execution cannot settle downward.
func AssessUsage(usage *Usage, uncertain, unsupported bool) UsageAssessment {
	assessment := UsageAssessment{AccountedTokens: ReservedTokensPerCall, StopExecution: unsupported}
	if usage != nil {
		if usage.CachedTokens != nil && (*usage.CachedTokens < 0 || usage.PromptTokens == nil || *usage.CachedTokens > *usage.PromptTokens) {
			assessment.StopExecution = true
		}
		if usage.PromptTokens != nil && *usage.PromptTokens > MaxInputTokens || usage.CompletionTokens != nil && *usage.CompletionTokens > MaxOutputTokens || usage.TotalTokens != nil && *usage.TotalTokens > MaxInputTokens+MaxOutputTokens {
			assessment.StopExecution = true
		}
	}
	if uncertain || assessment.StopExecution || usage == nil || usage.PromptTokens == nil || usage.CompletionTokens == nil || usage.TotalTokens == nil {
		return assessment
	}
	input, output, total := *usage.PromptTokens, *usage.CompletionTokens, *usage.TotalTokens
	if input < 1 || output < 0 || total != input+output {
		return assessment
	}
	assessment.AccountedTokens, assessment.InputTokens, assessment.OutputTokens = total, &input, &output
	return assessment
}
