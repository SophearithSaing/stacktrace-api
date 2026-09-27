package app

import "testing"

func TestGenerationDenialAllowlist(t *testing.T) {
	for reason, want := range map[GenerationDenial]GenerationJobStatus{
		GenerationContextRejected: JobSkipped, GenerationPublicationDenied: JobSkipped,
		GenerationPolicyDenied: JobSkipped, GenerationSourceRemoved: JobCancelled,
		GenerationExecutionCancelled: JobCancelled, GenerationOutputUnavailable: JobFailed,
	} {
		if got, valid := reason.TerminalStatus(); !valid || got != want {
			t.Fatal(reason, got, valid)
		}
	}
	for _, reason := range []GenerationDenial{"", "succeeded", "retry_wait", "provider_transient", "secret arbitrary error"} {
		if got, valid := reason.TerminalStatus(); valid || got != "" {
			t.Fatal(reason, got, valid)
		}
	}
}
