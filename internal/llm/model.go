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

	// Local construction/usage targets, not proof of hosted input tokenization
	// or a provider latency SLA. Every request uses n=1, max_tokens=1024, no
	// reasoning/tools and context_length_exceeded_behavior=error.
	RequestTimeout  = 45 * time.Second
	MaxInputTokens  = 8192
	MaxOutputTokens = 1024

	// Financial reservation relies on the documented provider context ceiling
	// plus the explicit maximum output, independently of our local estimate.
	// This is a reviewed provider-contract assumption, not a billing guarantee.
	ReservedTokensPerCall = ContextWindowTokens + MaxOutputTokens
)

func ValidateModel(provider, model string) error {
	if provider != Provider || model != Model {
		return fmt.Errorf("unsupported generation provider or model")
	}
	return nil
}

// TokenReservation checks local construction limits, then returns the full
// provider-ceiling reservation for every call, including tiny prompts and retries.
// localInputEstimate is NOT a verified bound on hosted input or financial cost.
// The output limit is fixed; retaining this parameter also checks command preflight.
func TokenReservation(localInputEstimate, maxOutputTokens int64) (int64, error) {
	if localInputEstimate < 1 || localInputEstimate > MaxInputTokens || maxOutputTokens != MaxOutputTokens {
		return 0, fmt.Errorf("generation token limits exceed local bounds")
	}
	return ReservedTokensPerCall, nil
}

// Usage matches Together's non-streaming usage object. Pointers distinguish
// missing/null fields from actual zero counts. Never infer zero cost from absent
// usage, a transport timeout or a failed request.
type Usage struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	TotalTokens      *int64 `json:"total_tokens"`
}
