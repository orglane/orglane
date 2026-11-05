package provider

import (
	"errors"
	"fmt"
	"time"
)

// Errors adapters return.
var (
	// ErrBadSignature: no candidate secret verifies the webhook signature.
	ErrBadSignature = errors.New("provider: bad webhook signature")
	// ErrNotFound: the provider-side object does not exist.
	ErrNotFound = errors.New("provider: not found")
	// ErrForeignDelivery: authentic, but for another installation or
	// organization.
	ErrForeignDelivery = errors.New("provider: delivery is not for this instance")
)

// CodeProviderError is the code of failures that are not an *Error.
const CodeProviderError = "provider_error"

// Error classifies provider failures for retries and results. Adapters
// return *Error for anything the user or admin should see; any other error
// is treated as retryable with code CodeProviderError. Provider response
// bodies are never copied into errors (they can echo tokens or personal
// data).
type Error struct {
	// Code is stable, lower_snake_case: "rate_limited", "seat_limit",
	// "owner_protected", ...
	Code      string
	Retryable bool
	// RetryAfter is from Retry-After / rate limit reset, if known.
	RetryAfter time.Duration
	// Err is the wrapped cause; it must not contain secrets or response
	// bodies.
	Err error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("provider: %s: %v", e.Code, e.Err)
	}
	return "provider: " + e.Code
}

func (e *Error) Unwrap() error { return e.Err }

// Classify returns the code, retryability and retry delay of err: those of
// an *Error in its chain, otherwise CodeProviderError, retryable.
func Classify(err error) (code string, retryable bool, retryAfter time.Duration) {
	var pe *Error
	if errors.As(err, &pe) {
		code = pe.Code
		if code == "" {
			code = CodeProviderError
		}
		return code, pe.Retryable, pe.RetryAfter
	}
	return CodeProviderError, true, 0
}
