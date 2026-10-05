package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

type togetherRoundTrip func(*http.Request) (*http.Response, error)

func (f togetherRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const togetherTestKey = "test-only-secret-credential"
const togetherTestContent = `{"decision":"publish","body":"Small transactions keep database locks short."}`
const togetherTestUsage = `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"cached_tokens":0}`

func togetherCompletion(content string) string {
	encoded, _ := json.Marshal(content)
	return `{"id":"completion-1","object":"chat.completion","model":"` + Model + `","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","tool_calls":[],"content":` + string(encoded) + `}}],"usage":` + togetherTestUsage + `}`
}

func togetherFake(t *testing.T, transport togetherRoundTrip) *Together {
	t.Helper()
	client, err := NewTogether(togetherTestKey)
	if err != nil {
		t.Fatal(err)
	}
	client.client.Transport = transport
	return client
}

func togetherResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func checkTogetherOutcome(t *testing.T, outcome app.GenerationOutcome, failure app.GenerationFailure, known bool) {
	t.Helper()
	if err := outcome.Validate(); err != nil {
		t.Fatalf("invalid domain outcome: %v", err)
	}
	if outcome.Failure != failure || (outcome.InputTokens != nil && outcome.OutputTokens != nil) != known {
		t.Fatalf("failure=%s known=%t, want %s known=%t", outcome.Failure, outcome.InputTokens != nil, failure, known)
	}
	if strings.Contains(fmt.Sprintf("%+v", outcome), togetherTestKey) {
		t.Fatal("credential escaped in outcome")
	}
}

func TestTogetherRequest(t *testing.T) {
	request := generationRequest(t, "Discuss Go.")
	prompt, err := BuildPrompt(request)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := togetherFake(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "POST" || r.URL.String() != togetherURL || r.GetBody != nil || r.Header.Get("Authorization") != "Bearer "+togetherTestKey || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" {
			t.Fatal("wrong fixed HTTP request")
		}
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > RequestTimeout || time.Until(deadline) < RequestTimeout-time.Second {
			t.Fatal("missing fixed deadline")
		}
		data, err := io.ReadAll(r.Body)
		if err != nil || len(data) > maxTogetherRequestBytes || strings.Contains(string(data), togetherTestKey) {
			t.Fatal("unsafe request body")
		}
		var schema any
		if json.Unmarshal([]byte(prompt.Schema()), &schema) != nil {
			t.Fatal("bad fixture schema")
		}
		want := map[string]any{
			"model": Model, "n": float64(1), "max_tokens": float64(1024), "stream": false,
			"context_length_exceeded_behavior": "error",
			"messages":                         []any{map[string]any{"role": "system", "content": prompt.System()}, map[string]any{"role": "user", "content": prompt.User()}},
			"response_format":                  map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "generation_result", "schema": schema}},
		}
		var got map[string]any
		if json.Unmarshal(data, &got) != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("request differed from exact prompt/schema contract")
		}
		return togetherResponse(200, togetherCompletion(togetherTestContent)), nil
	})
	outcome := client.Generate(context.Background(), request)
	checkTogetherOutcome(t, outcome, "", true)
	if calls != 1 || outcome.Result.Decision() != app.GenerationPublish || outcome.ProviderRequestID != "completion-1" || *outcome.InputTokens != 100 || *outcome.OutputTokens != 20 {
		t.Fatal("wrong successful completion")
	}
	if client.client.Timeout != RequestTimeout {
		t.Fatal("missing client timeout")
	}
}

func TestTogetherLocalRejection(t *testing.T) {
	for _, key := range []string{"", " secret", "secret\r\nInjected: yes", "秘密", strings.Repeat("s", 4097)} {
		client, err := NewTogether(key)
		if client != nil || err == nil || err.Error() != "invalid Together API key" {
			t.Fatal("unsafe credential validation")
		}
	}
	client := togetherFake(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("local rejection called provider")
		return nil, nil
	})
	checkTogetherOutcome(t, client.Generate(context.Background(), app.GenerationRequest{}), app.GenerationConfiguration, false)
	base, _ := BuildPrompt(generationRequest(t, "x"))
	oversized := generationRequest(t, strings.Repeat("x", 2+MaxInputTokens-int(base.LocalInputTokenEstimate())))
	checkTogetherOutcome(t, client.Generate(context.Background(), oversized), app.GenerationConfiguration, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	checkTogetherOutcome(t, client.Generate(ctx, generationRequest(t, "Discuss Go.")), app.GenerationCancelled, false)
}

func TestTogetherObservedLlamaResponse(t *testing.T) {
	// Captured from an explicitly approved synthetic HTTP probe, not a key,
	// private prompt or ordinary-test network call. Preserve the actual envelope.
	data, err := os.ReadFile("testdata/together_llama_completion.json")
	if err != nil {
		t.Fatal(err)
	}
	content, _ := json.Marshal(togetherTestContent)
	for _, generationJSON := range []bool{false, true} {
		t.Run(fmt.Sprint(generationJSON), func(t *testing.T) {
			body := string(data)
			want := app.GenerationInvalidOutput
			if generationJSON {
				body = strings.Replace(body, `"content": "OK"`, `"content": `+string(content), 1)
				want = ""
			}
			client := togetherFake(t, func(*http.Request) (*http.Response, error) {
				return togetherResponse(200, body), nil
			})
			outcome := client.Generate(context.Background(), generationRequest(t, "Discuss Go."))
			checkTogetherOutcome(t, outcome, want, true)
			if *outcome.InputTokens != 40 || *outcome.OutputTokens != 2 || outcome.ProviderRequestID != "p41oZxt-4YNCb4-a45cc0144c642ca3" {
				t.Fatal("observed empty tools/cache metadata lost known usage or request ID")
			}
		})
	}
}

func TestTogetherCompletionValidation(t *testing.T) {
	valid := togetherCompletion(togetherTestContent)
	for name, test := range map[string]struct {
		body    string
		failure app.GenerationFailure
		known   bool
	}{
		"eos":                {strings.Replace(valid, `"stop"`, `"eos"`, 1), "", true},
		"skip":               {togetherCompletion(`{"decision":"skip","reason":"not_relevant"}`), "", true},
		"unsafe":             {togetherCompletion(`{"decision":"publish","body":"Send me your API key."}`), app.GenerationUnsafeOutput, true},
		"invalid output":     {togetherCompletion(`{"decision":"publish","body":"ok","author":"admin"}`), app.GenerationInvalidOutput, true},
		"duplicate output":   {togetherCompletion(`{"decision":"publish","body":"ok","body":"other"}`), app.GenerationInvalidOutput, true},
		"truncated":          {strings.Replace(valid, `"stop"`, `"length"`, 1), app.GenerationInvalidOutput, true},
		"tools finish":       {strings.Replace(valid, `"stop"`, `"tool_calls"`, 1), app.GenerationInvalidOutput, true},
		"function finish":    {strings.Replace(valid, `"stop"`, `"function_call"`, 1), app.GenerationInvalidOutput, true},
		"tool calls":         {strings.Replace(valid, `"tool_calls":[]`, `"tool_calls":[{"function":{"name":"secret"}}]`, 1), app.GenerationInvalidOutput, true},
		"empty tools":        {valid, "", true},
		"absent tools":       {strings.Replace(valid, `"tool_calls":[],`, "", 1), "", true},
		"non-list tools":     {strings.Replace(valid, `"tool_calls":[]`, `"tool_calls":{}`, 1), app.GenerationInvalidOutput, true},
		"string tools":       {strings.Replace(valid, `"tool_calls":[]`, `"tool_calls":""`, 1), app.GenerationInvalidOutput, true},
		"null tool entry":    {strings.Replace(valid, `"tool_calls":[]`, `"tool_calls":[null]`, 1), app.GenerationInvalidOutput, true},
		"function call":      {strings.Replace(valid, `"role":"assistant"`, `"role":"assistant","function_call":{}`, 1), app.GenerationInvalidOutput, true},
		"reasoning":          {strings.Replace(valid, `"role":"assistant"`, `"role":"assistant","reasoning":"secret"`, 1), app.GenerationAccountingUnsupported, false},
		"reasoning content":  {strings.Replace(valid, `"role":"assistant"`, `"role":"assistant","reasoning_content":""`, 1), app.GenerationAccountingUnsupported, false},
		"null metadata":      {strings.Replace(strings.Replace(valid, `"role":"assistant"`, `"role":"assistant","reasoning":null`, 1), `"tool_calls":[]`, `"tool_calls":null`, 1), "", true},
		"wrong role":         {strings.Replace(valid, `"assistant"`, `"user"`, 1), app.GenerationInvalidOutput, true},
		"wrong model":        {strings.Replace(valid, Model, "another-model", 1), app.GenerationAccountingUnsupported, false},
		"missing model":      {strings.Replace(valid, `"model":"`+Model+`",`, "", 1), app.GenerationAccountingUnsupported, false},
		"bad index":          {strings.Replace(valid, `"index":0`, `"index":1`, 1), app.GenerationInvalidOutput, false},
		"null content":       {strings.Replace(valid, `"content":`, `"content":null,"ignored":`, 1), app.GenerationInvalidOutput, false},
		"no choices":         {`{"model":"` + Model + `","choices":[],"usage":` + togetherTestUsage + `}`, app.GenerationInvalidOutput, false},
		"multiple choices":   {`{"model":"` + Model + `","choices":[{},{}],"usage":` + togetherTestUsage + `}`, app.GenerationInvalidOutput, false},
		"malformed":          {valid[:len(valid)-2], app.GenerationInvalidOutput, false},
		"trailing":           {valid + ` {}`, app.GenerationInvalidOutput, false},
		"utf8":               {strings.Replace(valid, "Small", string([]byte{0xff}), 1), app.GenerationInvalidOutput, false},
		"duplicate envelope": {strings.Replace(valid, `"model":`, `"model":"other","model":`, 1), app.GenerationInvalidOutput, false},
		"aliased envelope":   {strings.Replace(valid, `"choices":`, `"Choices":`, 1), app.GenerationInvalidOutput, false},
		"escaped duplicate":  {strings.Replace(valid, `"model":`, `"\u006dodel":"other","model":`, 1), app.GenerationInvalidOutput, false},
		"duplicate choice":   {strings.Replace(valid, `"index":0`, `"index":0,"index":0`, 1), app.GenerationInvalidOutput, false},
		"aliased choice":     {strings.Replace(valid, `"finish_reason":`, `"Finish_reason":`, 1), app.GenerationInvalidOutput, false},
		"duplicate message":  {strings.Replace(valid, `"role":"assistant"`, `"role":"assistant","role":"assistant"`, 1), app.GenerationInvalidOutput, false},
		"aliased message":    {strings.Replace(valid, `"content":`, `"Content":`, 1), app.GenerationInvalidOutput, false},
	} {
		t.Run(name, func(t *testing.T) {
			client := togetherFake(t, func(*http.Request) (*http.Response, error) { return togetherResponse(200, test.body), nil })
			outcome := client.Generate(context.Background(), generationRequest(t, "Discuss Go."))
			checkTogetherOutcome(t, outcome, test.failure, test.known)
			if name == "skip" && outcome.Result.Decision() != app.GenerationSkip {
				t.Fatal("lost skip decision")
			}
		})
	}
}

func TestTogetherRepetition(t *testing.T) {
	request := generationRequest(t, "Discuss Go.")
	public := app.PublicGenerationContext{RecentAgentContent: []app.GenerationContextItem{{Author: app.GenerationContextAuthor{Handle: "go_agent", Type: app.AccountAgent}, Kind: app.OutputPost, Content: app.Content{Body: "Small transactions keep database locks short."}}}}
	var err error
	request.Context, err = app.BuildGenerationContext(request.Job, app.Persona{AgentID: request.Job.AgentID, Version: 1, Instructions: "Discuss Go.", TopicTags: []string{"go"}, CreatedAt: request.Job.CreatedAt}, public)
	if err != nil {
		t.Fatal(err)
	}
	client := togetherFake(t, func(*http.Request) (*http.Response, error) {
		return togetherResponse(200, togetherCompletion(togetherTestContent)), nil
	})
	checkTogetherOutcome(t, client.Generate(context.Background(), request), app.GenerationRepeatedOutput, true)
}

func TestTogetherUsage(t *testing.T) {
	for name, test := range map[string]struct {
		usage   string
		failure app.GenerationFailure
		known   bool
	}{
		"known":              {togetherTestUsage, "", true},
		"legacy counters":    {`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}`, "", true},
		"cached prompt":      {`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"cached_tokens":80}`, "", true},
		"invalid cache":      {`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"cached_tokens":101}`, app.GenerationAccountingUnsupported, false},
		"absent":             {"", "", false},
		"null":               {`null`, "", false},
		"missing counts":     {`{}`, "", false},
		"partial":            {`{"prompt_tokens":100}`, "", false},
		"inconsistent":       {`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":121}`, "", false},
		"negative":           {`{"prompt_tokens":-1,"completion_tokens":20,"total_tokens":19}`, "", false},
		"over input":         {`{"prompt_tokens":8193}`, app.GenerationAccountingUnsupported, false},
		"over output":        {`{"completion_tokens":1025}`, app.GenerationAccountingUnsupported, false},
		"unknown accounting": {`{"reasoning_tokens":0}`, app.GenerationAccountingUnsupported, false},
		"details":            {`{"completion_tokens_details":null}`, app.GenerationAccountingUnsupported, false},
		"malformed":          {`"oops"`, app.GenerationAccountingUnsupported, false},
		"alias":              {`{"Prompt_tokens":100}`, app.GenerationAccountingUnsupported, false},
		"duplicate":          {`{"prompt_tokens":100,"prompt_tokens":1}`, app.GenerationAccountingUnsupported, false},
		"overflow":           {`{"prompt_tokens":9999999999999999999999999}`, app.GenerationAccountingUnsupported, false},
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(togetherCompletion(togetherTestContent), togetherTestUsage, test.usage, 1)
			if test.usage == "" {
				body = strings.Replace(togetherCompletion(togetherTestContent), `,"usage":`+togetherTestUsage, "", 1)
			}
			client := togetherFake(t, func(*http.Request) (*http.Response, error) { return togetherResponse(200, body), nil })
			checkTogetherOutcome(t, client.Generate(context.Background(), generationRequest(t, "Discuss Go.")), test.failure, test.known)
		})
	}
}

func TestTogetherHTTPFailuresAndSecrets(t *testing.T) {
	for _, status := range []int{201, 204, 301, 302, 303, 307, 308, 400, 401, 403, 404, 408, 422, 429, 500, 501, 502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			client := togetherFake(t, func(*http.Request) (*http.Response, error) {
				calls++
				response := togetherResponse(status, togetherTestKey+togetherCompletion(togetherTestContent))
				response.Header.Set("Location", "https://untrusted.example/"+togetherTestKey)
				response.Header.Set("X-Request-ID", togetherTestKey)
				response.Header.Set("Retry-After", "60")
				response.Header.Set("X-Ratelimit-Reset", "120")
				return response, nil
			})
			before := time.Now()
			outcome := client.Generate(context.Background(), generationRequest(t, "Discuss Go."))
			checkTogetherOutcome(t, outcome, ClassifyHTTPFailure(status), false)
			if calls != 1 || outcome.ProviderRequestID != "" || outcome.NotBefore.Before(before.Add(120*time.Second)) {
				t.Fatal("redirect/retry/unsafe metadata or shortened hint")
			}
		})
	}
	client := togetherFake(t, func(*http.Request) (*http.Response, error) {
		return nil, errors.New(togetherTestKey + " private response and prompt")
	})
	checkTogetherOutcome(t, client.Generate(context.Background(), generationRequest(t, "Discuss Go.")), app.GenerationTransient, false)
}

func TestTogetherRequestIDs(t *testing.T) {
	for _, id := range []string{"", "req_1-abc.DEF:2", strings.Repeat("a", 256), strings.Repeat("a", 257), "has space", "line\nbreak", "秘密", "a/b", togetherTestKey} {
		t.Run(fmt.Sprintf("bytes-%d-%q", len(id), id[:min(len(id), 12)]), func(t *testing.T) {
			encoded, _ := json.Marshal(id)
			body := strings.Replace(togetherCompletion(togetherTestContent), `"completion-1"`, string(encoded), 1)
			client := togetherFake(t, func(*http.Request) (*http.Response, error) { return togetherResponse(200, body), nil })
			outcome := client.Generate(context.Background(), generationRequest(t, "Discuss Go."))
			checkTogetherOutcome(t, outcome, "", true)
			want := id
			if len(id) > 256 || strings.ContainsAny(id, " /\n") || id == "秘密" || id == togetherTestKey {
				want = ""
			}
			if outcome.ProviderRequestID != want {
				t.Fatal("unsafe or lost request ID")
			}
		})
	}
}

func TestTogetherCancellationAndDeadline(t *testing.T) {
	request := generationRequest(t, "Discuss Go.")
	for _, cancellation := range []bool{true, false} {
		t.Run(fmt.Sprint(cancellation), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			client := togetherFake(t, func(r *http.Request) (*http.Response, error) {
				if cancellation {
					cancel()
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			want := app.GenerationTimeout
			if cancellation {
				want = app.GenerationCancelled
			}
			checkTogetherOutcome(t, client.Generate(ctx, request), want, false)
		})
	}
}

type togetherTrackedBody struct {
	reader io.Reader
	read   int
	closed bool
}

func (b *togetherTrackedBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}
func (b *togetherTrackedBody) Close() error { b.closed = true; return nil }

func TestTogetherBodyBoundsAndClose(t *testing.T) {
	for _, status := range []int{200, 302, 429, 503} {
		body := &togetherTrackedBody{reader: strings.NewReader(strings.Repeat("x", maxTogetherResponseBytes+100))}
		client := togetherFake(t, func(*http.Request) (*http.Response, error) {
			response := togetherResponse(status, "")
			response.Body = body
			return response, nil
		})
		outcome := client.Generate(context.Background(), generationRequest(t, "Discuss Go."))
		want := ClassifyHTTPFailure(status)
		if status == 200 {
			want = app.GenerationInvalidOutput
		}
		checkTogetherOutcome(t, outcome, want, false)
		if !body.closed || body.read != maxTogetherResponseBytes+1 {
			t.Fatal("response not bounded/closed")
		}
	}
	for _, extra := range []int{0, 1} {
		valid := togetherCompletion(togetherTestContent)
		body := &togetherTrackedBody{reader: strings.NewReader(valid + strings.Repeat(" ", maxTogetherResponseBytes-len(valid)+extra))}
		client := togetherFake(t, func(*http.Request) (*http.Response, error) {
			response := togetherResponse(200, "")
			response.Body = body
			return response, nil
		})
		want := app.GenerationFailure("")
		if extra == 1 {
			want = app.GenerationInvalidOutput
		}
		checkTogetherOutcome(t, client.Generate(context.Background(), generationRequest(t, "Discuss Go.")), want, extra == 0)
		if !body.closed || body.read != maxTogetherResponseBytes+extra {
			t.Fatal("boundary body not read/closed")
		}
	}
}

type togetherReaderFunc func([]byte) (int, error)

func (f togetherReaderFunc) Read(p []byte) (int, error) { return f(p) }

func TestTogetherInterruptedBody(t *testing.T) {
	request := generationRequest(t, "Discuss Go.")
	for _, status := range []int{401, 429} {
		for _, cancellation := range []bool{true, false} {
			ctx, cancel := context.WithCancel(context.Background())
			body := &togetherTrackedBody{reader: togetherReaderFunc(func([]byte) (int, error) {
				if cancellation {
					cancel()
					return 0, context.Canceled
				}
				return 0, errors.New(togetherTestKey + " private response")
			})}
			client := togetherFake(t, func(*http.Request) (*http.Response, error) {
				response := togetherResponse(status, "")
				response.Header.Set("Retry-After", "300")
				response.Header.Set("X-Request-ID", "safe-header-id")
				response.Body = body
				return response, nil
			})
			before := time.Now()
			outcome := client.Generate(ctx, request)
			cancel()
			want := app.GenerationTransient
			if cancellation {
				want = app.GenerationCancelled
			}
			if status == 401 {
				want = app.GenerationCredentials
			}
			checkTogetherOutcome(t, outcome, want, false)
			if !body.closed || outcome.ProviderRequestID != "safe-header-id" || outcome.NotBefore.Before(before.Add(300*time.Second)) {
				t.Fatal("interrupted body lost close or safe retry metadata")
			}
		}
	}
}

// Use the production HTTP transport against a local TLS server without changing
// the adapter URL. No public network traffic or real credentials are possible.
func togetherLocalServer(t *testing.T, handler http.HandlerFunc) *Together {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	client, err := NewTogether(togetherTestKey)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.client.Transport.(*http.Transport)
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = "example.com" // httptest's certificate, not the production host.
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	t.Cleanup(transport.CloseIdleConnections)
	return client
}

func TestTogetherRealTransportHeaderLimit(t *testing.T) {
	client := togetherLocalServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Large", strings.Repeat("x", maxTogetherHeaderBytes+1))
		_, _ = io.WriteString(w, togetherCompletion(togetherTestContent))
	})
	checkTogetherOutcome(t, client.Generate(context.Background(), generationRequest(t, "Discuss Go.")), app.GenerationTransient, false)
}

func TestTogetherRealTransportBodyDeadline(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	client := togetherLocalServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-release
	})
	client.client.Timeout = 50 * time.Millisecond
	checkTogetherOutcome(t, client.Generate(context.Background(), generationRequest(t, "Discuss Go.")), app.GenerationTimeout, false)
}

func TestTogetherConcurrentCalls(t *testing.T) {
	request := generationRequest(t, "Discuss Go.")
	client := togetherFake(t, func(*http.Request) (*http.Response, error) {
		return togetherResponse(200, togetherCompletion(togetherTestContent)), nil
	})
	var group sync.WaitGroup
	for range 16 {
		group.Go(func() { checkTogetherOutcome(t, client.Generate(context.Background(), request), "", true) })
	}
	group.Wait()
}
