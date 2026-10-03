package llm

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"unicode/utf8"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// decodeTogetherCompletion strictly decodes a Together completion into a generation outcome.
func decodeTogetherCompletion(data []byte, request app.GenerationRequest) app.GenerationOutcome {
	outcome := app.GenerationOutcome{Failure: app.GenerationInvalidOutput}
	if !utf8.Valid(data) {
		return outcome
	}
	// Explicit metadata allowlists follow the documented non-streaming response.
	// Unknown/aliased fields fail closed instead of silently enabling new semantics.
	envelope, ok := togetherObject(data, "id", "object", "created", "model", "choices", "usage", "prompt", "warnings")
	if !ok {
		return outcome
	}
	usage, unsupported := DecodeUsage(envelope["usage"])
	assessment := AssessUsage(usage, false, unsupported)
	if assessment.StopExecution {
		outcome.Failure = app.GenerationAccountingUnsupported
		return outcome
	}
	// The completion ID is optional; Generate sanitizes it before returning it.
	outcome.ProviderRequestID, _ = togetherString(envelope["id"])
	model, ok := togetherString(envelope["model"])
	if !ok || model != Model {
		// Usage from a different model is outside the reviewed accounting contract.
		outcome.Failure = app.GenerationAccountingUnsupported
		return outcome
	}
	if object, exists := envelope["object"]; exists {
		if name, ok := togetherString(object); !ok || name != "chat.completion" {
			return outcome
		}
	}
	var choices []json.RawMessage
	if json.Unmarshal(envelope["choices"], &choices) != nil || len(choices) != 1 {
		return outcome
	}
	choice, ok := togetherObject(choices[0], "index", "finish_reason", "message", "text", "seed", "logprobs", "top_logprobs")
	if !ok || !bytes.Equal(bytes.TrimSpace(choice["index"]), []byte("0")) {
		return outcome
	}
	message, ok := togetherObject(choice["message"], "role", "content", "tool_calls", "function_call", "reasoning", "reasoning_content")
	if !ok {
		return outcome
	}
	outcome.InputTokens, outcome.OutputTokens = assessment.InputTokens, assessment.OutputTokens
	finish, _ := togetherString(choice["finish_reason"])
	completionFailure := CompletionFailure(finish,
		togetherPresent(message["tool_calls"]) || togetherPresent(message["function_call"]),
		togetherPresent(message["reasoning"]) || togetherPresent(message["reasoning_content"]))
	if completionFailure != "" {
		outcome.Failure = completionFailure
		if completionFailure == app.GenerationAccountingUnsupported {
			outcome.InputTokens, outcome.OutputTokens = nil, nil
		}
		return outcome
	}
	role, _ := togetherString(message["role"])
	content, ok := togetherString(message["content"])
	if role != "assistant" || !ok {
		return outcome
	}
	result, err := app.DecodeGenerationResult([]byte(content), request.Job)
	if err != nil {
		return outcome
	}
	// Context was built and bound locally, not supplied by the provider. Extract
	// only public recent content for the same heuristic used at publication.
	var public app.PublicGenerationContext
	if json.Unmarshal([]byte(request.Context.PublicJSON()), &public) != nil {
		return outcome
	}
	recent := make([]app.Content, 0, len(public.RecentAgentContent))
	for _, item := range public.RecentAgentContent {
		recent = append(recent, item.Content)
	}
	switch app.ValidateGenerationSafety(result, recent) {
	case nil:
		outcome.Result, outcome.Failure = result, ""
	case app.ErrGenerationUnsafe:
		outcome.Failure = app.GenerationUnsafeOutput
	case app.ErrGenerationRepetition:
		outcome.Failure = app.GenerationRepeatedOutput
	}
	return outcome
}

// togetherObject avoids encoding/json's last-key-wins and case folding. Values
// stay raw until their own bounded semantic decoder checks them.
func togetherObject(data []byte, allowed ...string) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, false
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || fields[name] != nil || !slices.Contains(allowed, name) {
			return nil, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		fields[name] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || decoder.Decode(new(any)) != io.EOF {
		return nil, false
	}
	return fields, true
}

// togetherString decodes a JSON string value without accepting null.
func togetherString(data []byte) (string, bool) {
	var value string
	if !togetherPresent(data) || json.Unmarshal(data, &value) != nil {
		return "", false
	}
	return value, true
}

// togetherPresent reports whether a JSON value is present and non-null.
func togetherPresent(data []byte) bool {
	return len(data) != 0 && !bytes.Equal(bytes.TrimSpace(data), []byte("null"))
}
