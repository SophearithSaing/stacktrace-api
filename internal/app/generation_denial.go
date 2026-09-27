package app

// GenerationDenial is a terminal worker acknowledgement, not spend, retry or
// publication authority. Only these fixed reasons may be supplied by a worker.
type GenerationDenial string

const (
	GenerationContextRejected    GenerationDenial = "context_rejected"
	GenerationPublicationDenied  GenerationDenial = "publication_denied"
	GenerationSourceRemoved      GenerationDenial = "source_removed"
	GenerationPolicyDenied       GenerationDenial = "policy_denied"
	GenerationOutputUnavailable  GenerationDenial = "outcome_unavailable"
	GenerationExecutionCancelled GenerationDenial = "execution_cancelled"
)

func (d GenerationDenial) TerminalStatus() (GenerationJobStatus, bool) {
	switch d {
	case GenerationContextRejected, GenerationPublicationDenied, GenerationPolicyDenied:
		return JobSkipped, true
	case GenerationSourceRemoved, GenerationExecutionCancelled:
		return JobCancelled, true
	case GenerationOutputUnavailable:
		return JobFailed, true
	default:
		return "", false
	}
}
