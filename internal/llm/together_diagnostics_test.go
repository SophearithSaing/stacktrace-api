package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestTogetherPrivateHTTPDiagnostics(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"Schema root must be an object; oneOf is unsupported.","type":"invalid_request_error","private":"never print envelope"}}`,
		`{"error":"Schema root must be an object; oneOf is unsupported."}`,
		`{"message":"Schema root must be an object; oneOf is unsupported."}`,
	} {
		var output bytes.Buffer
		provider, err := NewTogetherWithDiagnostics(togetherTestKey, &output)
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		provider.client.Transport = togetherRoundTrip(func(*http.Request) (*http.Response, error) {
			calls++
			response := togetherResponse(http.StatusBadRequest, body)
			response.Header.Set("X-Request-ID", "request-123")
			return response, nil
		})
		outcome := provider.Generate(context.Background(), generationRequest(t, "Private persona instructions."))
		checkTogetherOutcome(t, outcome, app.GenerationConfiguration, false)
		want := "Together HTTP 400 request_id=\"request-123\" message=\"Schema root must be an object; oneOf is unsupported.\"\n"
		if output.String() != want || calls != 1 {
			t.Fatalf("diagnostics=%q calls=%d", output.String(), calls)
		}
		encoded, _ := json.Marshal(outcome)
		if strings.Contains(string(encoded), "Schema root") || strings.Contains(output.String(), "never print envelope") {
			t.Fatal("provider message escaped its private sink or envelope was printed")
		}
	}
}

func TestTogetherDiagnosticsRedactPromptAndKeyBeforeBounding(t *testing.T) {
	request := generationRequest(t, "Private persona instructions.")
	prompt, err := BuildPrompt(request)
	if err != nil {
		t.Fatal(err)
	}
	escaped, _ := json.Marshal(prompt.System())
	message := "Schema problem:\n\x1b[31m " + togetherTestKey + " " + prompt.System() + " " + prompt.User() +
		" " + request.Context.Instructions() + " " + string(escaped[1:len(escaped)-1]) + "\u202e"
	encoded, _ := json.Marshal(map[string]any{"error": map[string]string{"message": message}})
	var output bytes.Buffer
	provider, _ := NewTogetherWithDiagnostics(togetherTestKey, &output)
	provider.client.Transport = togetherRoundTrip(func(*http.Request) (*http.Response, error) {
		response := togetherResponse(http.StatusBadRequest, string(encoded))
		response.Header.Set("X-Request-ID", togetherTestKey)
		return response, nil
	})
	checkTogetherOutcome(t, provider.Generate(context.Background(), request), app.GenerationConfiguration, false)
	for _, forbidden := range []string{togetherTestKey, "Private persona instructions.", prompt.System(), prompt.User(), "\x1b", "\u202e"} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("private or terminal-control text escaped: %q", forbidden)
		}
	}
	if !strings.Contains(output.String(), "[redacted]") || strings.Count(output.String(), "\n") != 1 {
		t.Fatalf("unsafe diagnostic framing: %q", output.String())
	}
	long := strings.Repeat("a", maxTogetherDiagnosticMessageBytes-5) + togetherTestKey + strings.Repeat("界", 1000)
	bounded := provider.redactDiagnosticMessage(long, prompt, request)
	if len(bounded) > maxTogetherDiagnosticMessageBytes+len(" [truncated]") || !strings.HasSuffix(bounded, " [truncated]") || strings.Contains(bounded, togetherTestKey[:5]) {
		t.Fatal("redaction must precede bounded truncation")
	}
}

func TestTogetherDiagnosticsNeverDumpUnreadableBodiesOrSuccesses(t *testing.T) {
	for _, body := range []string{
		"<html>" + togetherTestKey + " private proxy response</html>",
		`{"error":{"message":"` + togetherTestKey,
		`{"error":{"message":"` + string([]byte{0xff}) + `"}}`,
		strings.Repeat("x", maxTogetherResponseBytes+1),
		`{"error":{"private":"private response"}}`,
	} {
		var output bytes.Buffer
		provider, _ := NewTogetherWithDiagnostics(togetherTestKey, &output)
		provider.client.Transport = togetherRoundTrip(func(*http.Request) (*http.Response, error) {
			return togetherResponse(http.StatusBadRequest, body), nil
		})
		checkTogetherOutcome(t, provider.Generate(context.Background(), generationRequest(t, "Discuss Go.")), app.GenerationConfiguration, false)
		if !strings.Contains(output.String(), "error message unavailable") || strings.Contains(output.String(), togetherTestKey) || strings.Contains(output.String(), "private response") {
			t.Fatal("raw/unreadable error body escaped")
		}
	}
	var output bytes.Buffer
	provider, _ := NewTogetherWithDiagnostics(togetherTestKey, &output)
	provider.client.Transport = togetherRoundTrip(func(*http.Request) (*http.Response, error) {
		return togetherResponse(http.StatusOK, togetherCompletion(togetherTestContent)), nil
	})
	checkTogetherOutcome(t, provider.Generate(context.Background(), generationRequest(t, "Discuss Go.")), "", true)
	if output.Len() != 0 {
		t.Fatal("completion body leaked into diagnostics")
	}
}

type failedDiagnosticWriter struct{}

func (failedDiagnosticWriter) Write([]byte) (int, error) {
	return 0, errors.New("private writer failure")
}

func TestTogetherDiagnosticWriterFailureDoesNotRetryOrChangeAccounting(t *testing.T) {
	provider, _ := NewTogetherWithDiagnostics(togetherTestKey, failedDiagnosticWriter{})
	calls := 0
	provider.client.Transport = togetherRoundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		return togetherResponse(http.StatusUnauthorized, `{"error":{"message":"Invalid API key"}}`), nil
	})
	checkTogetherOutcome(t, provider.Generate(context.Background(), generationRequest(t, "Discuss Go.")), app.GenerationCredentials, false)
	if calls != 1 {
		t.Fatal("diagnostic failure retried provider execution")
	}
}

func TestTogetherCompletionRejectionDiagnostics(t *testing.T) {
	base := togetherCompletion(togetherTestContent)
	for name, test := range map[string]struct {
		body      string
		rejection string
		metadata  string
	}{
		"unknown usage field": {
			strings.Replace(base, togetherTestUsage, `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"reasoning_tokens":0}`, 1),
			"usage_unknown_field", "reasoning_tokens",
		},
		"duplicate usage field": {
			strings.Replace(base, togetherTestUsage, `{"prompt_tokens":100,"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}`, 1),
			"usage_duplicate_field", "prompt_tokens",
		},
		"usage scalar type": {
			strings.Replace(base, togetherTestUsage, `{"prompt_tokens":"`+togetherTestKey+`","completion_tokens":20,"total_tokens":120}`, 1),
			"usage_count_type_invalid", "string",
		},
		"token limit": {
			strings.Replace(base, togetherTestUsage, `{"prompt_tokens":8193,"completion_tokens":0,"total_tokens":8193}`, 1),
			"usage_token_limits_exceeded", "8193",
		},
		"model mismatch": {
			strings.Replace(base, Model, "unexpected-provider-model", 1),
			"model_mismatch", "unexpected-provider-model",
		},
		"model missing": {
			strings.Replace(base, `"model":"`+Model+`",`, "", 1),
			"model_missing_or_invalid", "missing or invalid",
		},
		"nonempty reasoning": {
			strings.Replace(base, `"role":"assistant"`, `"role":"assistant","reasoning":"`+togetherTestKey+`"`, 1),
			"reasoning_metadata_present", "reasoning_bytes",
		},
		"empty reasoning": {
			strings.Replace(base, `"role":"assistant"`, `"role":"assistant","reasoning_content":""`, 1),
			"reasoning_metadata_present", "reasoning_content_bytes",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			provider, _ := NewTogetherWithDiagnostics(togetherTestKey, &output)
			calls := 0
			provider.client.Transport = togetherRoundTrip(func(*http.Request) (*http.Response, error) {
				calls++
				return togetherResponse(200, test.body), nil
			})
			outcome := provider.Generate(context.Background(), generationRequest(t, "Private persona instructions."))
			checkTogetherOutcome(t, outcome, app.GenerationAccountingUnsupported, false)
			if calls != 1 || !strings.Contains(output.String(), "Together HTTP 200") || !strings.Contains(output.String(), `rejection="`+test.rejection+`"`) || !strings.Contains(output.String(), test.metadata) {
				t.Fatalf("missing precise rejection metadata: %q", output.String())
			}
			for _, secret := range []string{togetherTestKey, "Private persona instructions.", "Small transactions keep database locks short."} {
				if strings.Contains(output.String(), secret) {
					t.Fatal("scalar text, prompt or completion body escaped diagnostics")
				}
			}
			encoded, _ := json.Marshal(outcome)
			if strings.Contains(string(encoded), test.rejection) || strings.Contains(string(encoded), "cached_tokens") || strings.Contains(string(encoded), "unexpected-provider-model") {
				t.Fatal("private rejection detail escaped into the domain outcome")
			}
		})
	}
}

func TestTogetherCompletionMetadataBoundsAndRedaction(t *testing.T) {
	usage := make(map[string]any)
	for index := range 100 {
		usage[strings.Repeat("a", index+1)] = "private scalar text"
	}
	usage[togetherTestKey] = "private scalar text"
	encodedUsage, _ := json.Marshal(usage)
	body := strings.Replace(togetherCompletion(togetherTestContent), togetherTestUsage, string(encodedUsage), 1)
	body = strings.Replace(body, Model, togetherTestKey+"\u202e", 1)
	var output bytes.Buffer
	provider, _ := NewTogetherWithDiagnostics(togetherTestKey, &output)
	provider.client.Transport = togetherRoundTrip(func(*http.Request) (*http.Response, error) {
		return togetherResponse(200, body), nil
	})
	checkTogetherOutcome(t, provider.Generate(context.Background(), generationRequest(t, "Discuss Go.")), app.GenerationAccountingUnsupported, false)
	if len(output.String()) > 4*maxTogetherDiagnosticMessageBytes || strings.Contains(output.String(), togetherTestKey) || strings.Contains(output.String(), "private scalar text") || strings.Contains(output.String(), "\u202e") || strings.Count(output.String(), "\n") != 1 {
		t.Fatal("unbounded/private/control text in structural diagnostics")
	}
	metadata := togetherCompletionMetadata([]byte(body))
	if len(metadata["usage_fields"].(map[string]string)) > 16 {
		t.Fatal("unbounded usage field listing")
	}
}
