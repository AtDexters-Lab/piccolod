package app

// ValidationError represents a structured app manifest validation failure.
// It is intended to be surfaced to callers as a stable error code + message.
type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func newValidationError(code, message string) error {
	return &ValidationError{Code: code, Message: message}
}
