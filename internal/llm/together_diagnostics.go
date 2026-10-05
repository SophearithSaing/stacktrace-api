package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

const maxTogetherDiagnosticMessageBytes = 2048

// reportCompletionFailure explains an HTTP 200 rejection on the private sink.
// It exposes bounded structural metadata, never completion or reasoning text.
func (t *Together) reportCompletionFailure(requestID string, failure app.GenerationFailure, rejection string, data []byte, prompt Prompt, request app.GenerationRequest) {
	if t.diagnostics == nil {
		return
	}
	metadata, _ := json.Marshal(togetherCompletionMetadata(data))
	_, _ = fmt.Fprintf(t.diagnostics, "Together HTTP 200 request_id=%q adapter_failure=%q rejection=%q metadata=%q\n",
		requestID, failure, rejection, t.redactDiagnosticMessage(string(metadata), prompt, request))
}

// togetherCompletionMetadata extracts only bounded field/type names, scalar
// token counts, model identity, finish reason and reasoning-presence information.
func togetherCompletionMetadata(data []byte) map[string]any {
	metadata := make(map[string]any)
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil {
		metadata["response_json_valid"] = false
		return metadata
	}
	metadata["model"] = togetherDiagnosticString(envelope["model"])
	var usage map[string]json.RawMessage
	if json.Unmarshal(envelope["usage"], &usage) == nil && usage != nil {
		metadata["usage_field_count"] = len(usage)
		names := make([]string, 0, len(usage))
		for name := range usage {
			names = append(names, name)
		}
		sort.Strings(names)
		fields := make(map[string]string)
		for _, name := range names[:min(len(names), 16)] {
			if len(name) > 64 {
				fields["[oversized field name]"] = "withheld"
				continue
			}
			fields[name] = togetherDiagnosticType(usage[name])
		}
		metadata["usage_fields"] = fields
		counts := make(map[string]int64)
		for _, name := range []string{"prompt_tokens", "completion_tokens", "total_tokens", "cached_tokens"} {
			var count int64
			if togetherPresent(usage[name]) && json.Unmarshal(usage[name], &count) == nil {
				counts[name] = count
			}
		}
		metadata["token_counts"] = counts
	} else {
		metadata["usage_type"] = togetherDiagnosticType(envelope["usage"])
	}
	var choices []json.RawMessage
	if json.Unmarshal(envelope["choices"], &choices) == nil && len(choices) == 1 {
		var choice map[string]json.RawMessage
		if json.Unmarshal(choices[0], &choice) == nil {
			metadata["finish_reason"] = togetherDiagnosticString(choice["finish_reason"])
			var message map[string]json.RawMessage
			if json.Unmarshal(choice["message"], &message) == nil {
				for _, name := range []string{"reasoning", "reasoning_content"} {
					metadata[name+"_present"] = togetherPresent(message[name])
					var reasoning string
					if togetherPresent(message[name]) && json.Unmarshal(message[name], &reasoning) == nil {
						metadata[name+"_bytes"] = len(reasoning)
					}
				}
			}
		}
	}
	return metadata
}

// togetherDiagnosticString returns a bounded scalar model/finish label or a
// fixed placeholder; diagnostic redaction and quoting happen before output.
func togetherDiagnosticString(data []byte) string {
	value, ok := togetherString(data)
	if !ok {
		return "[missing or invalid]"
	}
	if len(value) > 128 {
		return "[oversized value withheld]"
	}
	return value
}

// togetherDiagnosticType reports a JSON type without displaying scalar data.
func togetherDiagnosticType(data []byte) string {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return "missing"
	}
	if bytes.Equal(data, []byte("null")) {
		return "null"
	}
	switch data[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	default:
		return "number"
	}
}

// reportHTTPFailure writes only a bounded error message to an opted-in private
// operator sink. Full bodies, headers and transport errors are never printed.
func (t *Together) reportHTTPFailure(status int, requestID string, data []byte, readErr error, prompt Prompt, request app.GenerationRequest) {
	if t.diagnostics == nil {
		return
	}
	message := "error message unavailable (non-JSON, malformed, oversized or unreadable response)"
	if readErr == nil && len(data) <= maxTogetherResponseBytes && utf8.Valid(data) {
		if decoded := togetherErrorMessage(data); decoded != "" {
			message = t.redactDiagnosticMessage(decoded, prompt, request)
		}
	}
	// Quoting escapes terminal controls and newlines. Output failure must not
	// change settlement, trigger a retry, or turn unknown usage into known usage.
	_, _ = fmt.Fprintf(t.diagnostics, "Together HTTP %d request_id=%q message=%q\n", status, requestID, message)
}

// togetherErrorMessage extracts only the provider's JSON message, not its envelope.
func togetherErrorMessage(data []byte) string {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil {
		return ""
	}
	if message, ok := togetherString(envelope["error"]); ok {
		return message
	}
	var detail map[string]json.RawMessage
	if json.Unmarshal(envelope["error"], &detail) == nil {
		if message, ok := togetherString(detail["message"]); ok {
			return message
		}
	}
	message, _ := togetherString(envelope["message"])
	return message
}

// redactDiagnosticMessage removes known credentials and complete prompt/persona
// echoes before bounding the terminal message; it is not a general PII detector.
func (t *Together) redactDiagnosticMessage(message string, prompt Prompt, request app.GenerationRequest) string {
	for _, secret := range []string{t.apiKey, prompt.System(), prompt.User(), request.Context.Instructions(), request.Context.PublicJSON(), generationInstructions} {
		if secret == "" {
			continue
		}
		message = strings.ReplaceAll(message, secret, "[redacted]")
		encoded, _ := json.Marshal(secret)
		message = strings.ReplaceAll(message, string(encoded[1:len(encoded)-1]), "[redacted]")
	}
	if len(message) > maxTogetherDiagnosticMessageBytes {
		message = message[:maxTogetherDiagnosticMessageBytes]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
		message += " [truncated]"
	}
	return message
}
