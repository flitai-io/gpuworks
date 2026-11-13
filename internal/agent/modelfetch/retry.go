package modelfetch

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"strings"
	"syscall"
	"time"
)

// RetryPolicy is how often and how patiently one address is retried after a
// transient transport failure before the fetch moves on.
type RetryPolicy struct {
	// MaxAttempts bounds the attempts at an address while another address
	// remains to move to.
	MaxAttempts int
	// LastAttempts bounds them at the last address, where giving up ends the
	// download; stalls there are retried like any transient failure.
	LastAttempts   int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// DefaultRetry bridges a short outage of one address: domestic hubs
// occasionally refuse TLS handshakes for a minute while large sibling files
// finish, which the last address is given 16 attempts to ride out. With
// another address to move to, six keep a bad mirror from holding a download
// for long.
var DefaultRetry = RetryPolicy{MaxAttempts: 6, LastAttempts: 16, InitialBackoff: time.Second, MaxBackoff: 15 * time.Second}

// IsTransient reports a failure that clearly came from the HTTP transport
// and may pass on its own: worth another try at the same address. HTTP
// statuses, authentication, validation and filesystem errors are not.
func IsTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	// The caller checks its own context first, so a deadline here is an
	// HTTP-client or stall timeout, not the end of the task.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	for _, errno := range []syscall.Errno{syscall.ECONNRESET, syscall.ECONNABORTED, syscall.ECONNREFUSED, syscall.EPIPE} {
		if errors.Is(err, errno) {
			return true
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) { //nolint:staticcheck // SA1019: preserve retries for temporary errors from legacy download transports.
		return true
	}
	// Some HTTP/2 and TLS transport failures have no exported error type.
	// Keep this list deliberately narrow.
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"connection reset by peer",
		"server closed idle connection",
		"tls handshake timeout",
		"http2: server sent goaway",
		"stream error: stream id",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// isLocal reports a failure of this node's own disk: no address can fix it.
func isLocal(err error) bool {
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) || errors.Is(err, syscall.EROFS) {
		return true
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return true
	}
	var linkErr *os.LinkError
	return errors.As(err, &linkErr)
}

func wait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func nextBackoff(current, maximum time.Duration) time.Duration {
	if current <= 0 {
		return 0
	}
	next := current * 2
	if next < current {
		next = maximum
	}
	if maximum > 0 && next > maximum {
		return maximum
	}
	return next
}
