package llm

import (
	"bytes"
	"encoding/json"
	"io"
)

// DecodeUsage rejects duplicate, unknown and case-alias fields. Unknown token
// details may change billing semantics: the caller must stop for review. Missing
// or null counts are merely unknown, never zero. No untrusted data is returned
// in an error or diagnostic. The entire provider response must also be bounded.
func DecodeUsage(data []byte) (usage *Usage, unsupported bool) {
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil, false
	}
	if len(data) > 1024 {
		return nil, true
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, true
	}
	result := &Usage{}
	fields := map[string]**int64{"prompt_tokens": &result.PromptTokens, "completion_tokens": &result.CompletionTokens, "total_tokens": &result.TotalTokens}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] || fields[name] == nil {
			return nil, true
		}
		seen[name] = true
		if decoder.Decode(fields[name]) != nil {
			return nil, true
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, true
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, true
	}
	return result, false
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
