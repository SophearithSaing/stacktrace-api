package app

import (
	"strings"
	"testing"
)

func TestGenerationOutcomeProviderRequestID(t *testing.T) {
	for _, id := range []string{"", "req_1-ABC.def:2", strings.Repeat("a", 256)} {
		if (GenerationOutcome{Failure: GenerationPermanent, ProviderRequestID: id}).Validate() != nil {
			t.Fatal("rejected safe request ID")
		}
	}
	for _, id := range []string{strings.Repeat("a", 257), "a/b", "a b", "a\nb", "a\x00b", "秘密"} {
		if (GenerationOutcome{Failure: GenerationPermanent, ProviderRequestID: id}).Validate() == nil {
			t.Fatal("accepted unsafe request ID")
		}
	}
}
