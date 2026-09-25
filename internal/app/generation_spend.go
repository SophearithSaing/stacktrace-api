package app

// A nonnil Attempt is permission for exactly ONE new call, only after this
// operation committed. Denials never return an earlier attempt as permission.
// Reason is a fixed safe code; callers must not log prompt/context payloads.
type GenerationAdmission struct {
	Attempt *GenerationAttempt
	Reason  string
}
