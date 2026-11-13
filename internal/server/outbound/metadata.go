package outbound

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"
)

const metadataAttemptTimeout = 30 * time.Second

// MetadataClient bounds small metadata reads and retries transient failures of
// bodyless GET/HEAD requests. Redirect validation and proxy routing remain the
// caller's policies. Response bodies are not replayed after bytes reach a reader.
// Whole archive/model downloads must continue using their streaming clients.
func MetadataClient(base *http.Client) *http.Client {
	client := &http.Client{}
	if base != nil {
		*client = *base
	}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = &metadataTransport{base: transport, timeout: metadataAttemptTimeout}
	if client.Timeout == 0 {
		client.Timeout = 100 * time.Second
	}
	return client
}

type metadataTransport struct {
	base    http.RoundTripper
	timeout time.Duration
}

func (t *metadataTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t *metadataTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if (req.Method != http.MethodGet && req.Method != http.MethodHead) || (req.Body != nil && req.Body != http.NoBody) {
		return t.base.RoundTrip(req)
	}
	for attempt := 0; ; attempt++ {
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(req.Context(), t.timeout)
		resp, err := t.base.RoundTrip(req.Clone(ctx))
		delay, retry := metadataRetry(resp, err, attempt)
		if attempt >= 2 || !retry || req.Context().Err() != nil {
			if resp != nil && resp.Body != nil {
				resp.Body = &metadataBody{ReadCloser: resp.Body, cancel: cancel}
			} else {
				cancel()
			}
			return resp, err
		}
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		cancel()
		timer := time.NewTimer(delay)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}
	}
}

func metadataRetry(resp *http.Response, err error, attempt int) (time.Duration, bool) {
	delay := time.Duration(attempt+1) * 200 * time.Millisecond
	if err != nil {
		var network net.Error
		retry := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || (errors.As(err, &network) && (network.Timeout() || network.Temporary())) //nolint:staticcheck // SA1019: preserve metadata retries for temporary DNS failures from existing transports.
		return delay, retry
	}
	switch resp.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
	default:
		return 0, false
	}
	if value := resp.Header.Get("Retry-After"); value != "" {
		var wait time.Duration
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
			if seconds > 5 {
				return 0, false
			}
			if seconds > 0 {
				wait = time.Duration(seconds) * time.Second
			}
		} else if when, err := http.ParseTime(value); err == nil {
			wait = time.Until(when)
		}
		// Do not hammer a mirror that asks for a wait longer than an interactive
		// preview's retry budget. Return its response for the caller to report.
		if wait > 5*time.Second {
			return 0, false
		}
		if wait > delay {
			delay = wait
		}
	}
	return delay, true
}

type metadataBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *metadataBody) Close() error {
	defer b.cancel()
	return b.ReadCloser.Close()
}
