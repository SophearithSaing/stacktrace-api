package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

const (
	togetherURL = "https://api.together.ai/v1/chat/completions"
	// JSON escaping can expand the admitted 8192-byte construction sixfold.
	maxTogetherRequestBytes  = 64 * 1024
	maxTogetherResponseBytes = 128 * 1024
	maxTogetherHeaderBytes   = 16 * 1024
)

// Together performs one bounded, non-streaming call per Generate. It grants no
// admission authority: callers must first commit a durable reservation.
type Together struct {
	apiKey string
	client *http.Client
}

var _ app.GenerationProvider = (*Together)(nil)

// NewTogether validates credentials without making a request. Endpoint, model,
// limits and transport policy are fixed; tests inject only a private transport.
func NewTogether(apiKey string) (*Together, error) {
	if len(apiKey) == 0 || len(apiKey) > 4096 {
		return nil, errors.New("invalid Together API key")
	}
	for _, c := range apiKey {
		if c < 33 || c > 126 {
			return nil, errors.New("invalid Together API key")
		}
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true) // Avoid HTTP/2's automatic refused-stream replay.
	transport := &http.Transport{
		Protocols:              protocols,
		DialContext:            (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  RequestTimeout,
		MaxResponseHeaderBytes: maxTogetherHeaderBytes,
		DisableCompression:     true,
		// No reused connections means the standard transport cannot replay a
		// request after a stale-connection failure. There are no application retries.
		DisableKeepAlives: true,
	}
	return &Together{apiKey: apiKey, client: &http.Client{
		Transport:     transport,
		Timeout:       RequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

type togetherMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type togetherResponseFormat struct {
	Type       string             `json:"type"`
	JSONSchema togetherJSONSchema `json:"json_schema"`
}

type togetherJSONSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
}

// togetherRequestBody encodes a validated prompt as a Together request body.
func togetherRequestBody(prompt Prompt) ([]byte, error) {
	return json.Marshal(struct {
		Model                         string                 `json:"model"`
		Messages                      []togetherMessage      `json:"messages"`
		N                             int                    `json:"n"`
		MaxTokens                     int                    `json:"max_tokens"`
		Stream                        bool                   `json:"stream"`
		ContextLengthExceededBehavior string                 `json:"context_length_exceeded_behavior"`
		ResponseFormat                togetherResponseFormat `json:"response_format"`
	}{
		Model: Model, Messages: []togetherMessage{{"system", prompt.System()}, {"user", prompt.User()}},
		N: 1, MaxTokens: MaxOutputTokens, Stream: false, ContextLengthExceededBehavior: "error",
		ResponseFormat: togetherResponseFormat{Type: "json_schema", JSONSchema: togetherJSONSchema{
			Name: "generation_result", Schema: json.RawMessage(prompt.Schema())}},
	})
}

// Generate submits a generation request and returns its classified outcome.
func (t *Together) Generate(ctx context.Context, request app.GenerationRequest) app.GenerationOutcome {
	failure := func(code app.GenerationFailure) app.GenerationOutcome { return app.GenerationOutcome{Failure: code} }
	prompt, err := BuildPrompt(request)
	if err != nil {
		return failure(app.GenerationConfiguration)
	}
	if _, err := prompt.AdmissionReservation(); err != nil {
		return failure(app.GenerationConfiguration)
	}
	body, err := togetherRequestBody(prompt)
	if err != nil || len(body) > maxTogetherRequestBytes {
		return failure(app.GenerationConfiguration)
	}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return failure(togetherTransportFailure(ctx, ctx.Err()))
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, togetherURL, bytes.NewReader(body))
	if err != nil {
		return failure(app.GenerationConfiguration)
	}
	httpRequest.GetBody = nil // Never make the paid POST replayable.
	httpRequest.Header.Set("Authorization", "Bearer "+t.apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	response, err := t.client.Do(httpRequest)
	if err != nil {
		return failure(togetherTransportFailure(ctx, err))
	}
	defer response.Body.Close()
	requestID := t.safeRequestID(response.Header.Get("X-Request-ID"))
	notBefore := togetherRetryNotBefore(response.Header, time.Now())
	data, err := io.ReadAll(io.LimitReader(response.Body, maxTogetherResponseBytes+1))
	if response.StatusCode != http.StatusOK {
		code := ClassifyHTTPFailure(response.StatusCode)
		// Error bodies carry no usable accounting. An interrupted body must not
		// turn an already-known credential/configuration stop into a retry.
		if !code.StopsExecution() && (err != nil || ctx.Err() != nil) {
			code = togetherTransportFailure(ctx, err)
		}
		return app.GenerationOutcome{Failure: code, ProviderRequestID: requestID, NotBefore: notBefore}
	}
	if err != nil || ctx.Err() != nil {
		return app.GenerationOutcome{Failure: togetherTransportFailure(ctx, err),
			ProviderRequestID: requestID, NotBefore: notBefore}
	}
	if len(data) > maxTogetherResponseBytes {
		return app.GenerationOutcome{Failure: app.GenerationInvalidOutput,
			ProviderRequestID: requestID, NotBefore: notBefore}
	}
	outcome := decodeTogetherCompletion(data, request)
	if ctx.Err() != nil {
		return app.GenerationOutcome{Failure: togetherTransportFailure(ctx, ctx.Err()),
			ProviderRequestID: requestID, NotBefore: notBefore}
	}
	if requestID == "" {
		requestID = t.safeRequestID(outcome.ProviderRequestID)
	}
	outcome.ProviderRequestID = requestID
	if outcome.Failure != "" {
		outcome.NotBefore = notBefore
	}
	return outcome
}

// togetherTransportFailure classifies a Together transport failure.
func togetherTransportFailure(ctx context.Context, err error) app.GenerationFailure {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return app.GenerationCancelled
	}
	var networkError net.Error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) && networkError.Timeout() {
		return app.GenerationTimeout
	}
	return app.GenerationTransient
}

// safeRequestID returns a bounded provider request identifier safe for persistence.
func (t *Together) safeRequestID(id string) string {
	if id == "" || strings.Contains(id, t.apiKey) || (app.GenerationOutcome{Failure: app.GenerationPermanent, ProviderRequestID: id}).Validate() != nil {
		return ""
	}
	return id
}
