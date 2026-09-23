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

// AssessUsage checks the individual request bounds, not just their sum. A prompt
// over its reserved bound cannot borrow unused completion capacity. Missing,
// negative or inconsistent usage retains the full reservation. Unsupported or
// over-bound usage additionally stops execution for operator review. An uncertain
// remote outcome always retains the full reservation, even with partial usage.
func AssessUsage(inputBound, outputBound int64, usage *Usage, uncertain, unsupported bool) UsageAssessment {
	reserved, err := TokenReservation(inputBound, outputBound)
	assessment := UsageAssessment{AccountedTokens: reserved, StopExecution: unsupported || err != nil}
	if err != nil {
		return assessment
	}
	if usage != nil {
		if usage.PromptTokens != nil && *usage.PromptTokens > inputBound || usage.CompletionTokens != nil && *usage.CompletionTokens > outputBound || usage.TotalTokens != nil && *usage.TotalTokens > reserved {
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
