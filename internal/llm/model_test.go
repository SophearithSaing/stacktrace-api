package llm

import "testing"

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
	for _, pair := range [][2]int64{{1, MaxOutputTokens}, {MaxInputTokens, MaxOutputTokens}} {
		got, err := TokenReservation(pair[0], pair[1])
		if err != nil || got != 132096 {
			t.Fatalf("reservation = %d, %v", got, err)
		}
	}
	for _, pair := range [][2]int64{{0, MaxOutputTokens}, {1, 0}, {-1, MaxOutputTokens}, {1, -1}, {1, 1}, {MaxInputTokens + 1, MaxOutputTokens}, {1, MaxOutputTokens + 1}, {1<<63 - 1, 1<<63 - 1}} {
		if _, err := TokenReservation(pair[0], pair[1]); err == nil {
			t.Fatal("accepted unbounded reservation")
		}
	}
}
