package llm

import (
	"encoding/json"
	"testing"
)

func TestSelectedModel(t *testing.T) {
	if err := ValidateModel(Provider, Model); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"", Model}, {Provider, ""}, {"other", Model}, {Provider, "meta-llama/Llama-3.1-8B-Instruct-Turbo"}} {
		if err := ValidateModel(pair[0], pair[1]); err == nil {
			t.Fatal("accepted unreviewed provider/model")
		}
	}
}

func TestTokenReservation(t *testing.T) {
	for _, pair := range [][2]int64{{1, 1}, {MaxInputTokens, MaxOutputTokens}} {
		got, err := TokenReservation(pair[0], pair[1])
		if err != nil || got != pair[0]+pair[1] {
			t.Fatalf("reservation = %d, %v", got, err)
		}
	}
	for _, pair := range [][2]int64{{0, 1}, {1, 0}, {-1, 1}, {1, -1}, {MaxInputTokens + 1, 1}, {1, MaxOutputTokens + 1}, {1<<63 - 1, 1<<63 - 1}} {
		if _, err := TokenReservation(pair[0], pair[1]); err == nil {
			t.Fatal("accepted unbounded reservation")
		}
	}
}

func TestUsageAccounting(t *testing.T) {
	reserved, err := TokenReservation(MaxInputTokens, MaxOutputTokens)
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		raw    string
		tokens int64
		known  bool
	}{
		"known":            {`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}`, 120, true},
		"zero output":      {`{"prompt_tokens":100,"completion_tokens":0,"total_tokens":100}`, 100, true},
		"full bound":       {`{"prompt_tokens":8192,"completion_tokens":1024,"total_tokens":9216}`, reserved, true},
		"null":             {`null`, reserved, false},
		"missing":          {`{}`, reserved, false},
		"partial":          {`{"prompt_tokens":100,"total_tokens":100}`, reserved, false},
		"null count":       {`{"prompt_tokens":100,"completion_tokens":null,"total_tokens":100}`, reserved, false},
		"negative":         {`{"prompt_tokens":100,"completion_tokens":-1,"total_tokens":99}`, reserved, false},
		"zero prompt":      {`{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`, reserved, false},
		"inconsistent":     {`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":121}`, reserved, false},
		"oversized prompt": {`{"prompt_tokens":8193,"completion_tokens":20,"total_tokens":8213}`, reserved, false},
		"oversized output": {`{"prompt_tokens":100,"completion_tokens":1025,"total_tokens":1125}`, reserved, false},
		"overflow":         {`{"prompt_tokens":9223372036854775807,"completion_tokens":9223372036854775807,"total_tokens":-2}`, reserved, false},
	} {
		t.Run(name, func(t *testing.T) {
			var usage *Usage
			if err := json.Unmarshal([]byte(test.raw), &usage); err != nil {
				t.Fatal(err)
			}
			tokens, known := AccountedTokens(reserved, usage)
			if tokens != test.tokens || known != test.known {
				t.Fatalf("accounted = %d, %t; want %d, %t", tokens, known, test.tokens, test.known)
			}
		})
	}
	input, output, total := int64(100), int64(20), int64(120)
	if tokens, known := AccountedTokens(110, &Usage{&input, &output, &total}); tokens != 110 || known {
		t.Fatalf("over-reservation usage released budget: %d, %t", tokens, known)
	}
}
