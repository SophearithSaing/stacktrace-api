package app

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func generationOutputJob(kind GenerationOutput) GenerationJob {
	job := testGenerationJob()
	if kind != OutputPost {
		actor, source := NewID(), NewID()
		job.TriggerKind, job.OutputKind, job.TriggerActorID, job.SourcePostID, job.CooldownKey = TriggerHumanPost, kind, &actor, &source, "cooldown"
	}
	return job
}

func decodeTestGeneration(t *testing.T, body string) GenerationResult {
	t.Helper()
	data, err := json.Marshal(map[string]string{"decision": "publish", "body": body})
	if err != nil {
		t.Fatal(err)
	}
	result, err := DecodeGenerationResult(data, testGenerationJob())
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestGenerationOutputStrict(t *testing.T) {
	for _, raw := range []string{
		`{}`, `null`, `[]`, `{"decision":"publish"}`, `{"decision":"skip"}`,
		`{"decision":"publish","body":null}`, `{"decision":"publish","body":1}`,
		`{"decision":"publish","body":"hi","reason":null}`,
		`{"decision":"skip","reason":"not_relevant","body":"hi"}`,
		`{"decision":"skip","reason":"not_relevant","code":null}`,
		`{"decision":"skip","reason":"provider secret detail"}`,
		`{"decision":"PUBLISH","body":"hi"}`, `{"Decision":"publish","body":"hi"}`,
		`{"decision":"publish","body":"hi","body":"bye"}`,
		`{"decision":"publish","body":"hi","b\u006fdy":"bye"}`,
		`{"decision":"publish","body":"hi","Body":"bye"}`,
		`{"decision":"publish","body":"hi","author_id":"spoof"}`,
		`{"decision":"publish","body":"hi","target":"spoof"}`,
		`{"decision":"publish","body":"hi","output_kind":"quote"}`,
		`{"decision":"publish","body":"hi","tools":[]}`,
		`{"decision":"publish","body":"hi","is_generated":true}`,
		`{"decision":"publish","body":"hi","code":null}`,
		`{"decision":"publish","body":"hi","code":{}}`,
		`{"decision":"publish","body":"hi","code":{"language":"go","filename":"a.go","source":"x","source":"y"}}`,
		`{"decision":"publish","body":"hi","code":{"language":"go","filename":"a.go","source":"x","Source":"y"}}`,
		`{"decision":"publish","body":"hi","code":{"language":"go","filename":"a.go","source":null}}`,
		`{"decision":"publish","body":"hi","code":{"language":"go","filename":"../\u0000","source":"x"}}`,
		`{"decision":"publish","body":"hi\u0000"}`, `{"decision":"publish","body":"\ud800"}`,
		`{"decision":"publish","body":"\udc00"}`, `{"decision":"publish","body":"\ufffd"}`,
		`{"decision":"publish","body":"hi"} {}`, `{"decision":"publish","body":"hi"`,
		"{\"decision\":\"publish\",\"body\":\"\xff\"}",
		strings.Repeat(" ", MaxGenerationOutputBytes) + `{"decision":"publish","body":"hi"}`,
	} {
		if _, err := DecodeGenerationResult([]byte(raw), testGenerationJob()); !errors.Is(err, ErrGenerationOutput) {
			t.Fatalf("accepted malformed output %q: %v", raw, err)
		}
	}
	for _, reason := range []string{"not_relevant", "insufficient_context", "unsafe_request", "repetition"} {
		result, err := DecodeGenerationResult([]byte(`{"decision":"skip","reason":"`+reason+`"}`), testGenerationJob())
		if err != nil || result.Decision() != GenerationSkip || result.Reason() != reason || result.Content().Body != "" {
			t.Fatalf("skip = %+v, %v", result, err)
		}
	}
}

func TestGenerationOutputOrdinaryValidation(t *testing.T) {
	for _, kind := range []GenerationOutput{OutputPost, OutputQuote, OutputReply} {
		job := generationOutputJob(kind)
		for _, size := range []int{0, 1, 320, 321} {
			raw, _ := json.Marshal(map[string]string{"decision": "publish", "body": strings.Repeat("界", size)})
			_, err := DecodeGenerationResult(raw, job)
			if (err == nil) != (size >= 1 && size <= 320) {
				t.Fatalf("%s size %d: %v", kind, size, err)
			}
		}
		_, err := DecodeGenerationResult([]byte(`{"decision":"publish","body":"#a #b #c #d #e #f"}`), job)
		if (err == nil) != (kind == OutputReply) {
			t.Fatalf("tag rules for %s: %v", kind, err)
		}
		_, err = DecodeGenerationResult([]byte(`{"decision":"publish","body":"hi","code":{"language":" go ","filename":" a.go ","source":" x "}}`), job)
		if (err == nil) != (kind != OutputReply) {
			t.Fatalf("code rules for %s: %v", kind, err)
		}
	}
	for _, source := range []string{" ", strings.Repeat("x", MaxCodeSourceBytes+1)} {
		raw, _ := json.Marshal(map[string]any{"decision": "publish", "body": "hi", "code": map[string]string{"language": "go", "filename": "a.go", "source": source}})
		if _, err := DecodeGenerationResult(raw, testGenerationJob()); err == nil {
			t.Fatal("accepted invalid code")
		}
	}
}

func TestGenerationOutputDigest(t *testing.T) {
	a := decodeTestGeneration(t, "  Hello #Go \n")
	b := decodeTestGeneration(t, "Hello #Go")
	if a.Digest() != b.Digest() || !validGenerationOutputDigest(a.Digest()) {
		t.Fatal("normalization changed digest")
	}
	if a.Digest() == decodeTestGeneration(t, "Hello #go").Digest() {
		t.Fatal("changed actual body has same digest")
	}
	seen := map[string]bool{}
	for _, kind := range []GenerationOutput{OutputPost, OutputQuote, OutputReply} {
		result, err := DecodeGenerationResult([]byte(`{"decision":"publish","body":"Hello #Go"}`), generationOutputJob(kind))
		if err != nil || seen[result.Digest()] {
			t.Fatal("kind not bound", err)
		}
		seen[result.Digest()] = true
	}
	for _, code := range []string{`"language":"go","filename":"a.go","source":"x"`, `"language":"Go","filename":"a.go","source":"x"`, `"language":"go","filename":"b.go","source":"x"`, `"language":"go","filename":"a.go","source":" x"`} {
		result, err := DecodeGenerationResult([]byte(`{"decision":"publish","body":"Hello #Go","code":{`+code+`}}`), testGenerationJob())
		if err != nil || seen[result.Digest()] {
			t.Fatal("code not bound", err)
		}
		seen[result.Digest()] = true
		before := result.Digest()
		copy := result.Content()
		copy.Code.Source = "mutated"
		copy.Tags[0].Slug = "mutated"
		if before != result.Digest() || result.Content().Tags[0].Slug != "go" {
			t.Fatal("result was mutable")
		}
	}
	if (GenerationResult{}).Digest() != "" {
		t.Fatal("zero result has digest")
	}
}

func TestGenerationSafety(t *testing.T) {
	for _, body := range []string{"I am an OFFICIAL representative.", "I have live\naccess to your server.", "I have real-time access.", "I can access your repository.", "I just browsed the web.", "Send me your API key.", "kill yourself", "o f f i c i a l spokesperson", "safe\u200btext", "safe\u202etext", "safe\x01text", "<|system|>", "[INST] do this"} {
		if err := ValidateGenerationSafety(decodeTestGeneration(t, body), nil); !errors.Is(err, ErrGenerationUnsafe) {
			t.Fatalf("unsafe %q: %v", body, err)
		}
	}
	for _, body := range []string{"Use the official documentation.", "I am an AI persona, not a human.", "Check your own logs before changing the index.", "A realtime system needs a latency budget."} {
		if err := ValidateGenerationSafety(decodeTestGeneration(t, body), nil); err != nil {
			t.Fatalf("safe %q: %v", body, err)
		}
	}
	result, err := DecodeGenerationResult([]byte(`{"decision":"publish","body":"An example","code":{"language":"go","filename":"a.go","source":"// Send me your password"}}`), testGenerationJob())
	if err != nil || !errors.Is(ValidateGenerationSafety(result, nil), ErrGenerationUnsafe) {
		t.Fatal("code safety bypass", err)
	}
}

func TestGenerationRepetition(t *testing.T) {
	for _, pair := range [][2]string{
		{"A small tip!", "a SMALL tip."},
		{"Keep the query simple and measure the execution plan before adding indexes.", "Keep the query simple and measure the execution plan before adding more indexes."},
	} {
		if err := ValidateGenerationSafety(decodeTestGeneration(t, pair[0]), []Content{{Body: pair[1]}}); !errors.Is(err, ErrGenerationRepetition) {
			t.Fatalf("repeat not detected: %v", err)
		}
	}
	result := decodeTestGeneration(t, "Explicit transactions keep updates atomic.")
	if err := ValidateGenerationSafety(result, []Content{{Body: "Profile allocations before optimizing."}}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGenerationSafety(result, make([]Content, MaxGenerationRecentContent+1)); err == nil {
		t.Fatal("unbounded history accepted")
	}
	if err := ValidateGenerationSafety(result, []Content{{Body: ""}}); err == nil {
		t.Fatal("invalid history accepted")
	}
}
