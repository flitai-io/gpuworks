// Package download provides a resumable, multi-connection HTTP range
// downloader with rate limiting and progress callbacks. It uses only the
// Go standard library.
package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultConcurrency = 4
	readBufSize        = 32 * 1024
	progressInterval   = 250 * time.Millisecond
	// requestTimeout is the longest one HTTP request may take end to end. The
	// standard library counts the whole body read against Client.Timeout, so a
	// range or a single-stream file that is still flowing gets cut here; the
	// cut is a transport timeout, which the model download retries from the
	// .part file, so a slow link costs a resume, not the bytes already on disk.
	requestTimeout = 15 * time.Minute
)

// stallTimeout is how long a request may deliver nothing (no response headers,
// or no body bytes) before it is abandoned and retried, so a CDN that accepts a
// range request and then stops sending bytes costs this long rather than the
// whole requestTimeout.
var stallTimeout = 5 * time.Minute

// Progress is reported periodically during a download.
type Progress struct {
	Total         int64 // total bytes (0 if unknown)
	Done          int64 // bytes downloaded so far
	RateBytesPerS int64 // recent throughput
}

// Options configures one file download.
type Options struct {
	URL          string
	Dest         string            // final file path; a "<Dest>.part" is used while downloading
	Concurrency  int               // number of parallel range connections (default 4); falls back to 1 if server lacks range support
	RateLimitKBs int               // 0 = unlimited (global across connections)
	Headers      map[string]string // extra request headers (e.g. Authorization)
	Client       *http.Client      // optional; defaults to a sane client
	// ResumeKey names the bytes rather than where they are fetched from: a
	// multi-connection download resumes its .part file only when the key it
	// was started under matches. Empty means URL. A caller fetching one
	// immutable file from several addresses gives every address the same
	// key, so a switch continues the bytes already on disk.
	ResumeKey string
	// StallTimeout replaces the package's stall limit; zero keeps it. Tests
	// of callers that act on stalls set it.
	StallTimeout time.Duration
}

func (o Options) stallTimeout() time.Duration {
	if o.StallTimeout > 0 {
		return o.StallTimeout
	}
	return stallTimeout
}

// StatusError is a server answering a file request with a status that
// cannot be the file.
type StatusError struct {
	Code   int
	Detail string
	// RetryAfter is the wait a throttled answer named (throttle.go); zero
	// when it named none or the answer is not throttled.
	RetryAfter time.Duration
}

func (e *StatusError) Error() string { return e.Detail }

func (o Options) resumeKey() string {
	if o.ResumeKey != "" {
		return o.ResumeKey
	}
	return o.URL
}

// Download fetches URL to Dest. It resumes from an existing "<Dest>.part" when
// the server supports ranges (Range probe). onProgress may be nil; when set it
// is called at most a few times per second. Honors ctx cancellation.
func Download(ctx context.Context, opts Options, onProgress func(Progress)) error {
	started := time.Now()
	logURL := safeLogURL(opts.URL)
	slog.Info("download started", "url", logURL, "dest", opts.Dest, "concurrency", opts.Concurrency)
	defer func() {
		slog.Debug("download finished", "url", logURL, "dest", opts.Dest, "duration", time.Since(started).String())
	}()
	if opts.URL == "" {
		slog.Warn("download rejected", "reason", "missing URL")
		return errors.New("download: URL is required")
	}
	if opts.Dest == "" {
		return errors.New("download: Dest is required")
	}
	if opts.Client == nil {
		opts.Client = defaultClient()
	} else {
		opts.Client = withRequestTimeout(opts.Client)
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = defaultConcurrency
	}

	partPath := opts.Dest + ".part"
	statePath := opts.Dest + ".part.state"

	pr, err := probe(ctx, opts)
	if err != nil {
		slog.Warn("download probe failed", "url", logURL, "err", err)
		return err
	}

	limiter := newRateLimiter(float64(opts.RateLimitKBs) * 1024)

	if pr.acceptRange && pr.size > 0 && opts.Concurrency > 1 {
		return multiDownload(ctx, opts, pr.size, partPath, statePath, limiter, onProgress)
	}
	return singleDownload(ctx, opts, pr, partPath, limiter, onProgress)
}

func safeLogURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid-url>"
	}
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return u.String()
}

// probeResult captures what the server told us about the resource.
type probeResult struct {
	size        int64 // -1 if unknown
	acceptRange bool
}

// probe issues a GET with "Range: bytes=0-0" to learn the total size and
// whether the server supports range requests.
func probe(ctx context.Context, opts Options) (probeResult, error) {
	resp, err := guardedGet(ctx, opts, "bytes=0-0")
	if err != nil {
		return probeResult{}, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
		resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		size := parseContentRangeTotal(resp.Header.Get("Content-Range"))
		return probeResult{size: size, acceptRange: size > 0}, nil
	case http.StatusOK:
		size := resp.ContentLength // -1 if unknown
		return probeResult{size: size, acceptRange: false}, nil
	default:
		return probeResult{}, statusError(resp, fmt.Sprintf("download: unexpected probe status %d", resp.StatusCode))
	}
}

// singleDownload streams the body over a single connection. It resumes by
// appending to an existing .part file when the server honors a Range start,
// and otherwise restarts from scratch.
func singleDownload(ctx context.Context, opts Options, pr probeResult, partPath string, limiter *rateLimiter, onProgress func(Progress)) error {
	var resumeFrom int64
	if fi, err := os.Stat(partPath); err == nil {
		resumeFrom = fi.Size()
	}

	rangeHeader := ""
	if resumeFrom > 0 {
		rangeHeader = "bytes=" + strconv.FormatInt(resumeFrom, 10) + "-"
	}
	resp, err := guardedGet(ctx, opts, rangeHeader)
	if err != nil {
		return err
	}
	if rangeHeader != "" && resp.StatusCode == http.StatusOK {
		// Some upstreams send only the suffix with status 200, even with a
		// full-file Content-Length. Do not truncate the old part using that
		// ambiguous body. Make one fresh request without Range instead.
		resp.Body.Close()
		fullOpts := opts
		fullOpts.Headers = make(map[string]string, len(opts.Headers))
		for key, value := range opts.Headers {
			if !strings.EqualFold(key, "Range") {
				fullOpts.Headers[key] = value
			}
		}
		resp, err = guardedGet(ctx, fullOpts, "")
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return statusError(resp, fmt.Sprintf("download: unexpected full response status %d", resp.StatusCode))
		}
		if resp.Header.Get("Content-Range") != "" {
			resp.Body.Close()
			return fmt.Errorf("download: full request returned Content-Range %q", resp.Header.Get("Content-Range"))
		}
	}
	defer resp.Body.Close()

	total := pr.size
	if total < 0 {
		total = 0
	}
	var done int64
	var f *os.File

	switch resp.StatusCode {
	case http.StatusPartialContent:
		f, err = os.OpenFile(partPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		done = resumeFrom
		if t := parseContentRangeTotal(resp.Header.Get("Content-Range")); t > 0 {
			total = t
		}
	case http.StatusOK:
		// Server ignored the Range (or none requested): full body, restart.
		f, err = os.OpenFile(partPath, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		done = 0
		if resp.ContentLength >= 0 {
			total = resp.ContentLength
		}
	default:
		return statusError(resp, fmt.Sprintf("download: unexpected status %d", resp.StatusCode))
	}
	defer f.Close()

	doneCtr := done
	rep := startReporter(ctx, total, func() int64 { return atomic.LoadInt64(&doneCtr) }, onProgress)
	err = streamCopy(ctx, writerAtWriter{f: f}, resp.Body, limiter, &doneCtr)
	rep.stop()
	if err != nil {
		return err
	}
	return finalize(f, partPath, opts.Dest, "")
}

// writerAtWriter adapts an append-mode *os.File to io.Writer for streamCopy.
type writerAtWriter struct{ f *os.File }

func (w writerAtWriter) Write(p []byte) (int, error) { return w.f.Write(p) }

// streamCopy copies from r to w, updating the done counter and honoring the
// rate limiter and context.
func streamCopy(ctx context.Context, w io.Writer, r io.Reader, limiter *rateLimiter, done *int64) error {
	buf := make([]byte, readBufSize)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := r.Read(buf)
		if n > 0 {
			if err := writeAll(w, buf[:n]); err != nil {
				return err
			}
			atomic.AddInt64(done, int64(n))
			if err := limiter.Wait(ctx, n); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// finalize fsyncs, closes, renames .part -> dest and removes the state sidecar.
func finalize(f *os.File, partPath, dest, statePath string) error {
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(partPath, dest); err != nil {
		return err
	}
	if statePath != "" {
		_ = os.Remove(statePath)
	}
	return nil
}

func applyHeaders(req *http.Request, h map[string]string) {
	for k, v := range h {
		req.Header.Set(k, v)
	}
}

// withRequestTimeout returns c with its overall timeout raised to
// requestTimeout. A caller's client (a node's proxy client, say) is built for
// short control requests; the downloader's own stall guard, not that timeout,
// decides whether a transfer has hung. A client without a timeout, or with a
// longer one, is returned unchanged.
func withRequestTimeout(c *http.Client) *http.Client {
	if c.Timeout == 0 || c.Timeout >= requestTimeout {
		return c
	}
	raised := *c
	raised.Timeout = requestTimeout
	return &raised
}

func defaultClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 100
	t.MaxIdleConnsPerHost = 16
	return &http.Client{Timeout: requestTimeout, Transport: t}
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// parseContentRangeTotal extracts the total size from a "Content-Range" header
// value like "bytes 0-0/12345". Returns 0 if unknown ("*") or unparsable.
func parseContentRangeTotal(v string) int64 {
	i := strings.LastIndex(v, "/")
	if i < 0 {
		return 0
	}
	tail := strings.TrimSpace(v[i+1:])
	if tail == "" || tail == "*" {
		return 0
	}
	n, err := strconv.ParseInt(tail, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
