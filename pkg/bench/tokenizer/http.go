package tokenizer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"
)

// ErrDownload distinguishes fetching a tokenizer from interpreting its content.
// Wrapped transport errors retain their DNS, timeout and cancellation identity.
var ErrDownload = errors.New("tokenizer download failed")

// errNotFound is a tokenizer file the server does not have (HTTP 404): a
// repository that ships its vocabulary under another name.
var errNotFound = errors.New("tokenizer file not found")

const maxTokenizerBytes = 64 << 20

func loadHTTP(parent context.Context, rawURL string) (*Tokenizer, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cachePath := tokenizerCachePath(rawURL)
	if tok := cachedTokenizer(cachePath); tok != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return tok, nil
	}
	for attempt := 0; ; attempt++ {
		data, retry, retryAfter, err := downloadTokenizer(ctx, rawURL)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err == nil {
			tok, err := LoadFile(bytes.NewReader(data))
			if err != nil {
				return nil, err
			}
			cacheTokenizer(cachePath, data)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return tok, nil
		}
		if !retry || attempt == 2 {
			return nil, fmt.Errorf("%w: %w", ErrDownload, err)
		}
		delay := time.Duration(attempt+1) * 200 * time.Millisecond
		if retryAfter > delay {
			delay = retryAfter
		}
		if deadline, ok := ctx.Deadline(); ok && retryAfter > 0 && retryAfter >= time.Until(deadline) {
			return nil, fmt.Errorf("%w: %w", ErrDownload, err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func downloadTokenizer(ctx context.Context, rawURL string) ([]byte, bool, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false, 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, transientDownload(err), 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		retry := resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode == 500 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504
		if resp.StatusCode == http.StatusNotFound {
			return nil, false, 0, fmt.Errorf("%w: HTTP %s", errNotFound, resp.Status)
		}
		return nil, retry, tokenizerRetryAfter(resp.Header.Get("Retry-After")), fmt.Errorf("HTTP %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenizerBytes+1))
	if err != nil {
		return nil, transientDownload(err), 0, err
	}
	if len(data) > maxTokenizerBytes {
		return nil, false, 0, fmt.Errorf("tokenizer download exceeds %d bytes", maxTokenizerBytes)
	}
	return data, false, 0, nil
}

// Match metadata GET retry behavior without depending on server-owned packages.
func transientDownload(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var network net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || (errors.As(err, &network) && (network.Timeout() || network.Temporary())) //nolint:staticcheck // SA1019: keep tokenizer retries for temporary DNS failures, matching metadata downloads.
}

// Retry-After may be delta seconds or an HTTP date. Cap long deltas at the
// entire load budget to avoid overflow; the caller compares the wait with its
// remaining deadline and returns the HTTP failure when another attempt cannot fit.
func tokenizerRetryAfter(value string) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		if seconds >= 30 {
			return 30 * time.Second
		}
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}
