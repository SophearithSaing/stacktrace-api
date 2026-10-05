package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func togetherSchemaRequest(t *testing.T, kind app.GenerationOutput) app.GenerationRequest {
	t.Helper()
	if kind == app.OutputPost {
		return generationRequest(t, "Discuss Go.")
	}
	job, persona, public := socialPromptFixture(t)
	job.OutputKind = kind
	input, err := app.BuildGenerationContext(job, persona, public)
	if err != nil {
		t.Fatal(err)
	}
	return app.GenerationRequest{Job: job, Context: input}
}

func TestTogetherGrammarCompatibleSchema(t *testing.T) {
	for _, kind := range []app.GenerationOutput{app.OutputPost, app.OutputReply, app.OutputQuote} {
		t.Run(string(kind), func(t *testing.T) {
			request := togetherSchemaRequest(t, kind)
			client := togetherFake(t, func(r *http.Request) (*http.Response, error) {
				var body struct {
					ResponseFormat struct {
						Type       string                     `json:"type"`
						JSONSchema map[string]json.RawMessage `json:"json_schema"`
					} `json:"response_format"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Fatal("invalid request JSON")
				}
				format := body.ResponseFormat
				if format.Type != "json_schema" || len(format.JSONSchema) != 2 || format.JSONSchema["strict"] != nil {
					t.Fatal("request must use Together's documented name/schema envelope without strict mode")
				}
				var schema struct {
					Type                 string                     `json:"type"`
					Properties           map[string]json.RawMessage `json:"properties"`
					Required             []string                   `json:"required"`
					AdditionalProperties bool                       `json:"additionalProperties"`
					OneOf                json.RawMessage            `json:"oneOf"`
					AnyOf                json.RawMessage            `json:"anyOf"`
				}
				if json.Unmarshal(format.JSONSchema["schema"], &schema) != nil {
					t.Fatal("invalid response schema")
				}
				// Reproduce the hosted rejection without making a paid request.
				if schema.Type != "object" || schema.Properties == nil || schema.OneOf != nil || schema.AnyOf != nil {
					return togetherResponse(422, `{"error":"Json object grammar must have a 'properties' field"}`), nil
				}
				if !reflect.DeepEqual(schema.Required, []string{"decision"}) || schema.AdditionalProperties {
					t.Fatal("invalid shared envelope constraints")
				}
				var decision struct {
					Type string   `json:"type"`
					Enum []string `json:"enum"`
				}
				if json.Unmarshal(schema.Properties["decision"], &decision) != nil || decision.Type != "string" || !reflect.DeepEqual(decision.Enum, []string{"publish", "skip"}) {
					t.Fatal("decision must be bounded to publish/skip")
				}
				var bodyRule struct {
					Min int `json:"minLength"`
					Max int `json:"maxLength"`
				}
				if json.Unmarshal(schema.Properties["body"], &bodyRule) != nil || bodyRule.Min != 1 || bodyRule.Max != 320 || schema.Properties["reason"] == nil {
					t.Fatal("lost bounded body/skip contract")
				}
				_, hasCode := schema.Properties["code"]
				propertyCount := 3
				if kind != app.OutputReply {
					propertyCount++
				}
				if hasCode != (kind != app.OutputReply) || len(schema.Properties) != propertyCount {
					t.Fatal("wrong job-specific properties or privileged fields")
				}
				return togetherResponse(200, togetherCompletion(togetherTestContent)), nil
			})
			checkTogetherOutcome(t, client.Generate(context.Background(), request), "", true)
		})
	}
}

func TestTogetherObjectSchemaKeepsExactLocalDecisionValidation(t *testing.T) {
	for _, kind := range []app.GenerationOutput{app.OutputPost, app.OutputReply, app.OutputQuote} {
		for name, content := range map[string]string{
			"publish":        `{"decision":"publish","body":"Use a bounded worker loop."}`,
			"skip":           `{"decision":"skip","reason":"not_relevant"}`,
			"code":           `{"decision":"publish","body":"Use a bounded worker loop.","code":{"language":"go","filename":"main.go","source":"package main"}}`,
			"mixed":          `{"decision":"publish","body":"Use a bounded worker loop.","reason":"not_relevant"}`,
			"mixed null":     `{"decision":"publish","body":"Use a bounded worker loop.","reason":null}`,
			"skip body":      `{"decision":"skip","reason":"not_relevant","body":"Use a bounded worker loop."}`,
			"skip code":      `{"decision":"skip","reason":"not_relevant","code":null}`,
			"missing body":   `{"decision":"publish"}`,
			"missing reason": `{"decision":"skip"}`,
			"privilege":      `{"decision":"publish","body":"Use a bounded worker loop.","author_id":"admin"}`,
			"duplicate":      `{"decision":"publish","decision":"skip","reason":"not_relevant"}`,
			"null code":      `{"decision":"publish","body":"Use a bounded worker loop.","code":null}`,
		} {
			t.Run(string(kind)+"/"+name, func(t *testing.T) {
				client := togetherFake(t, func(*http.Request) (*http.Response, error) {
					return togetherResponse(200, togetherCompletion(content)), nil
				})
				want := app.GenerationInvalidOutput
				if name == "publish" || name == "skip" || name == "code" && kind != app.OutputReply {
					want = ""
				}
				checkTogetherOutcome(t, client.Generate(context.Background(), togetherSchemaRequest(t, kind)), want, true)
			})
		}
	}
}
