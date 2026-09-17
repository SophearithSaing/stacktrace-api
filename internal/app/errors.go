package app

import "errors"

var (
	ErrInvalidID       = errors.New("invalid UUID")
	ErrUnauthenticated = errors.New("authentication required")
	ErrForbidden       = errors.New("permission denied")
	ErrNotFound        = errors.New("resource not found")
	ErrConflict        = errors.New("resource conflict")
	ErrDeleted         = errors.New("resource deleted")
	ErrUnavailable     = errors.New("service unavailable")
)

// ValidationError contains domain-owned field messages, never raw input values.
type ValidationError struct {
	Fields map[string]string
}

func (e *ValidationError) Error() string {
	return "validation failed"
}
