// Package llm defines the selected provider contract. It does not yet execute
// requests; no provider registry, credentials or HTTP adapter lives here.
package llm

import (
	"fmt"
	"time"
)

const (
	Provider = "together"
	Model    = "meta-llama/Llama-3.3-70B-Instruct-Turbo"

	// Catalog capabilities, not local request limits. This is a non-reasoning
	// baseline; adding a model requires a fresh accounting/capability review.
	ContextWindowTokens       = 131072
	SupportsStructuredOutputs = true

	// Local targets, not a provider latency SLA. The future context builder must
	// supply a conservative token upper bound including the chat template and
	// schema, not a characters/4 estimate. Send n=1 and max_tokens explicitly.
	RequestTimeout  = 45 * time.Second
	MaxInputTokens  = 8192
	MaxOutputTokens = 1024
)

func ValidateModel(provider, model string) error {
	if provider != Provider || model != Model {
		return fmt.Errorf("unsupported generation provider or model")
	}
	return nil
}

// TokenReservation validates local limits and returns the upper bound that must
// be reserved durably before each call, including retries of the same job.
func TokenReservation(inputUpperBound, maxOutputTokens int64) (int64, error) {
	if inputUpperBound < 1 || inputUpperBound > MaxInputTokens || maxOutputTokens < 1 || maxOutputTokens > MaxOutputTokens {
		return 0, fmt.Errorf("generation token limits exceed local bounds")
	}
	return inputUpperBound + maxOutputTokens, nil
}

// Usage matches Together's non-streaming usage object. Pointers distinguish
// missing/null fields from actual zero counts. Never infer zero cost from absent
// usage, a transport timeout or a failed request.
type Usage struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	TotalTokens      *int64 `json:"total_tokens"`
}

// AccountedTokens returns trusted known usage or the complete reservation. Only
// call this with a positive reservation returned by TokenReservation. Unknown
// execution outcomes must pass nil even if a partial response supplied usage.
// Unsupported reasoning/token detail also requires nil until reviewed. The
// adapter must alert on inconsistent or over-bound usage; never silently free
// budget based on it. These are budget accounting rules, not billing guarantees.
func AccountedTokens(reservedTokens int64, usage *Usage) (tokens int64, known bool) {
	if usage == nil || usage.PromptTokens == nil || usage.CompletionTokens == nil || usage.TotalTokens == nil {
		return reservedTokens, false
	}
	input, output, total := *usage.PromptTokens, *usage.CompletionTokens, *usage.TotalTokens
	if input < 1 || input > MaxInputTokens || output < 0 || output > MaxOutputTokens || total != input+output || total > reservedTokens {
		return reservedTokens, false
	}
	return total, true
}
