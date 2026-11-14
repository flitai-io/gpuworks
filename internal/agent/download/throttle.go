package download

import (
	"errors"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// Throttled reports whether the server answered "slow down" rather than
// that it cannot serve the file: 429, or 503, the same two statuses an image
// pull waits out (runtime/udocker/native). The answer is about the moment,
// not the address.
func (e *StatusError) Throttled() bool { return modelrepo.Throttles(e.Code) }

// IsThrottled reports whether err is a throttled answer, and how long the
// server asked the client to wait (zero when it did not say).
func IsThrottled(err error) (time.Duration, bool) {
	var status *StatusError
	if !errors.As(err, &status) || !status.Throttled() {
		return 0, false
	}
	return status.RetryAfter, true
}

// statusError is the error for a response whose status cannot be the file.
// A throttled answer carries the wait it names.
func statusError(resp *http.Response, detail string) *StatusError {
	e := &StatusError{Code: resp.StatusCode, Detail: detail}
	if e.Throttled() {
		// The rule model repository listings follow too.
		e.RetryAfter = modelrepo.ThrottleWait(resp.Header, time.Now())
	}
	return e
}
