package apps

import (
	"errors"
	"fmt"
)

// Errors returned by Service. Each has a stable problem code (Code).
var (
	ErrNotFound = errors.New("apps: not found")
	// ErrInvalid wraps validation errors with the offending field.
	ErrInvalid = errors.New("apps: invalid input")
	// ErrKeyReserved: another instance uses the key, or a removed
	// instance's data still holds it.
	ErrKeyReserved = errors.New("apps: the instance key is reserved")
	// ErrInvalidState: the instance is not in a state that allows the
	// operation.
	ErrInvalidState = errors.New("apps: the instance state does not allow this")
	// ErrNotSupported: the provider does not implement what the operation
	// needs.
	ErrNotSupported = errors.New("apps: the provider does not support this")
	// ErrProviderUnavailable: the instance's provider is not compiled into
	// this binary.
	ErrProviderUnavailable = errors.New("apps: the provider is not available")
	// ErrRotationInProgress: a credential already has a pending or
	// uncertain version.
	ErrRotationInProgress = errors.New("apps: a rotation of this credential is in progress")
	// ErrNoRotation: there is no pending or uncertain version to abandon.
	ErrNoRotation = errors.New("apps: no rotation of this credential is in progress")
	// ErrBatchChanged: the removal batch is no longer held or differs from
	// what the admin saw.
	ErrBatchChanged = errors.New("apps: the removal batch changed")
	// ErrForbidden: the caller may not do this (e.g. a person who is not
	// active opening an app).
	ErrForbidden = errors.New("apps: not allowed")
	// ErrNoQueue: the job queue is not attached (see SetQueue).
	ErrNoQueue = errors.New("apps: the job queue is not running")
)

// Code returns the stable problem code of a Service error, or "".
func Code(err error) string {
	for _, c := range []struct {
		err  error
		code string
	}{
		{ErrNotFound, "not_found"},
		{ErrInvalid, "invalid_input"},
		{ErrKeyReserved, "key_reserved"},
		{ErrInvalidState, "invalid_state"},
		{ErrNotSupported, "not_supported"},
		{ErrProviderUnavailable, "provider_unavailable"},
		{ErrRotationInProgress, "rotation_in_progress"},
		{ErrNoRotation, "no_rotation"},
		{ErrBatchChanged, "batch_changed"},
		{ErrForbidden, "forbidden"},
	} {
		if errors.Is(err, c.err) {
			return c.code
		}
	}
	return ""
}

func invalid(field, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalid, field, fmt.Sprintf(format, args...))
}
