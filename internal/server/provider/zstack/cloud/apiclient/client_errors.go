package apiclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
)

type ErrorKind string

const (
	ErrorNetwork        ErrorKind = "network error"
	ErrorTimeout        ErrorKind = "timeout"
	ErrorCanceled       ErrorKind = "canceled"
	ErrorAuthentication ErrorKind = "authentication rejected"
	ErrorRequest        ErrorKind = "request rejected"
	ErrorOperation      ErrorKind = "Cloud operation failed"
	ErrorResponse       ErrorKind = "invalid Cloud response"
)

// APIError preserves machine-readable failure information. OutcomeUnknown
// means a submitted mutation may still have executed; it must not be replayed
// merely because the caller stopped waiting. Remote descriptions are omitted.
type APIError struct {
	Kind           ErrorKind
	Phase          RequestPhase
	ViaProxy       bool
	AuthReason     AuthenticationReason
	StatusCode     int
	Code           string
	DiagnosticCode string
	JobID          string
	OutcomeUnknown bool
	cause          error
	retryable      bool
	definitive     bool
}

func (e *APIError) Error() string {
	message := "zstack: " + string(e.Kind)
	if e.StatusCode != 0 {
		message += fmt.Sprintf(" (HTTP %d)", e.StatusCode)
	}
	if e.OutcomeUnknown {
		message += "; operation outcome unknown"
	}
	return message
}

func (e *APIError) Unwrap() error { return e.cause }

func transportError(err error) *APIError {
	kind := ErrorNetwork
	var timeout net.Error
	switch {
	case errors.Is(err, context.Canceled):
		kind = ErrorCanceled
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()):
		kind = ErrorTimeout
	}
	retryable := kind == ErrorTimeout || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE)
	if errors.As(err, &timeout) && timeout.Temporary() { //nolint:staticcheck // SA1019: existing API transports report temporary network failures through this legacy interface.
		retryable = true
	}
	return &APIError{Kind: kind, cause: err, retryable: retryable && kind != ErrorCanceled}
}

// A stopped retry loop must retain the last attempt's diagnostic evidence,
// while cancellation or the total deadline remains the terminal error kind.
func interruptedRequestError(stop error, last *APIError) *APIError {
	terminal := transportError(stop)
	if last == nil {
		return terminal
	}
	copy := *last
	copy.Kind = terminal.Kind
	copy.cause = errors.Join(last, stop)
	copy.retryable = false
	return &copy
}

func jobError(err error, jobID string) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		apiErr = &APIError{Kind: ErrorResponse, cause: err}
	}
	copy := *apiErr
	copy.JobID = jobID
	// A job lookup error (including 404 or lost authentication) says nothing
	// about the submitted mutation. Only a Cloud operation result settles it.
	copy.OutcomeUnknown = !(copy.Kind == ErrorOperation && copy.definitive &&
		(copy.StatusCode == http.StatusOK || copy.StatusCode == http.StatusServiceUnavailable))
	return &copy
}
