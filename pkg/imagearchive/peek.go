package imagearchive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	// tailWindow is the first Range request: `docker save` writes manifest.json
	// plus the legacy repositories entry into the last few KiB.
	tailWindow = 64 << 10
	// tailWindowLarge is the single retry when manifest.json straddles the
	// first window (many RepoTags on one image).
	tailWindowLarge = 1 << 20
	// headWindow covers the OCI-layout index.json, written near the head.
	headWindow = 64 << 10
	// maxFetchBytes caps everything this package reads from the archive.
	maxFetchBytes = 1200 << 10
	// probeBytes is the tiny Range GET used to test servers that do not
	// advertise Accept-Ranges but honour Range anyway.
	probeBytes = 16
	// maxRedirects bounds redirect chains (decision 11 of the plan).
	maxRedirects = 5
)

// ErrInvalidURL marks a URL this package refuses before any request is made.
var ErrInvalidURL = errors.New("invalid archive url")

// UpstreamStatusError is an HTTP status from the archive server. The error
// text is only the method and status, never the URL: query strings can carry
// credentials.
type UpstreamStatusError struct {
	Method string
	Status int
}

func (e *UpstreamStatusError) Error() string {
	if e == nil {
		return "unexpected status"
	}
	return fmt.Sprintf("%s: unexpected status %d", e.Method, e.Status)
}

// errRangeIgnored is the internal signal that the server answered a Range
// request with the full body (200 instead of 206).
var errRangeIgnored = errors.New("server ignored range request")

// Peek is what can be learned about a remote image archive without
// downloading it.
type Peek struct {
	// SizeBytes is the archive size reported by Content-Length.
	SizeBytes int64
	// RepoTags are the image references baked into the archive: the
	// docker-archive manifest.json RepoTags, or the OCI index.json
	// org.opencontainers.image.ref.name annotations.
	RepoTags []string
	// Previewable reports whether RepoTags could be determined. A false value
	// is not an error: compressed archives and servers without Range support
	// simply cannot be inspected ahead of the download.
	Previewable bool
}

// PeekURL reads an image archive's size and repository tags from rawURL,
// fetching at most ~1.2 MiB. client must not be nil; PeekURL never mutates it.
func PeekURL(ctx context.Context, client *http.Client, rawURL string) (Peek, error) {
	target, err := parseArchiveURL(rawURL)
	if err != nil {
		return Peek{}, err
	}
	if client == nil {
		return Peek{}, errors.New("imagearchive: nil http client")
	}
	// Work on a shallow copy so the caller's client keeps its own policy and is
	// never mutated from under a concurrent user.
	c := *client
	if c.CheckRedirect == nil {
		c.CheckRedirect = limitRedirects
	}
	f := &fetcher{client: &c, url: target.String(), remaining: maxFetchBytes}

	size, acceptsRanges, err := f.head(ctx)
	if err != nil {
		return Peek{}, err
	}
	peek := Peek{SizeBytes: size}
	if !acceptsRanges {
		// Some servers omit Accept-Ranges but still honour Range; ask for a few
		// bytes to find out instead of trusting the header.
		ok, err := f.probeRange(ctx, size)
		if err != nil {
			return Peek{}, err
		}
		if !ok {
			return peek, nil
		}
	}

	tail, err := f.tailGet(ctx, size, tailWindow)
	if err != nil {
		if errors.Is(err, errRangeIgnored) {
			return peek, nil
		}
		return Peek{}, err
	}
	whole := int64(len(tail)) >= size
	scan := scanTail(tail)
	if len(scan.tags) == 0 && scan.needMore() && !whole && size > tailWindow {
		big, err := f.tailGet(ctx, size, tailWindowLarge)
		if err != nil {
			if errors.Is(err, errRangeIgnored) {
				return peek, nil
			}
			return Peek{}, err
		}
		tail, whole = big, int64(len(big)) >= size
		scan = scanTail(big)
	}
	if len(scan.tags) > 0 {
		peek.RepoTags, peek.Previewable = scan.tags, true
		return peek, nil
	}

	head := tail
	if !whole {
		want := int64(headWindow)
		if want > size {
			want = size
		}
		head, err = f.rangeGet(ctx, 0, want-1)
		if err != nil {
			if errors.Is(err, errRangeIgnored) {
				return peek, nil
			}
			return Peek{}, err
		}
	}
	if isGzip(head) {
		// A compressed archive cannot be inspected by Range; not an error.
		return peek, nil
	}
	if tags, ok := ScanHead(head); ok {
		peek.RepoTags, peek.Previewable = tags, true
	}
	return peek, nil
}

// parseArchiveURL enforces decision 11: http/https only, no embedded
// credentials.
func parseArchiveURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	if err := checkURL(u); err != nil {
		return nil, err
	}
	return u, nil
}

func checkURL(u *url.URL) error {
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("%w: scheme %q is not http or https", ErrInvalidURL, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: missing host", ErrInvalidURL)
	}
	if u.User != nil {
		return fmt.Errorf("%w: embedded credentials are not allowed", ErrInvalidURL)
	}
	return nil
}

// CheckRedirect is the redirect policy every request against an operator
// supplied archive URL must use: at most maxRedirects hops, and the same
// scheme/credential rules re-applied to each hop's target so a redirect cannot
// walk the fetch onto a scheme the caller rejected.
func CheckRedirect(req *http.Request, via []*http.Request) error {
	return limitRedirects(req, via)
}

// HeadSize reports the archive's size with a single HEAD request. It is the
// cheap half of PeekURL, for callers that only need a byte count (disk
// admission and progress) and not the repository tags.
func HeadSize(ctx context.Context, client *http.Client, rawURL string) (int64, error) {
	target, err := parseArchiveURL(rawURL)
	if err != nil {
		return 0, err
	}
	if client == nil {
		return 0, errors.New("imagearchive: nil http client")
	}
	c := *client
	if c.CheckRedirect == nil {
		c.CheckRedirect = limitRedirects
	}
	f := &fetcher{client: &c, url: target.String(), remaining: maxFetchBytes}
	size, _, err := f.head(ctx)
	return size, err
}

func limitRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	return checkURL(req.URL)
}

// fetcher performs the bounded set of requests PeekURL is allowed to make.
type fetcher struct {
	client    *http.Client
	url       string
	remaining int64 // bytes still allowed to be read from the archive
}

func (f *fetcher) head(ctx context.Context) (size int64, acceptsRanges bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, f.url, nil)
	if err != nil {
		return 0, false, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer closeBody(resp)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, false, &UpstreamStatusError{Method: http.MethodHead, Status: resp.StatusCode}
	}
	size = resp.ContentLength
	if size < 0 {
		size = parseContentLength(resp.Header.Get("Content-Length"))
	}
	if size <= 0 {
		return 0, false, fmt.Errorf("HEAD %s: missing or empty Content-Length", f.url)
	}
	acceptsRanges = strings.Contains(strings.ToLower(resp.Header.Get("Accept-Ranges")), "bytes")
	return size, acceptsRanges, nil
}

func parseContentLength(v string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// probeRange reports whether the server honours Range even though it did not
// advertise Accept-Ranges.
func (f *fetcher) probeRange(ctx context.Context, size int64) (bool, error) {
	end := int64(probeBytes) - 1
	if end > size-1 {
		end = size - 1
	}
	if _, err := f.rangeGet(ctx, 0, end); err != nil {
		if errors.Is(err, errRangeIgnored) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// tailGet fetches the last window bytes of the archive.
func (f *fetcher) tailGet(ctx context.Context, size, window int64) ([]byte, error) {
	start := size - window
	if start < 0 {
		start = 0
	}
	return f.rangeGet(ctx, start, size-1)
}

// rangeGet fetches [start,end] inclusive, requiring a 206 answer and staying
// inside the package's byte budget.
func (f *fetcher) rangeGet(ctx context.Context, start, end int64) ([]byte, error) {
	want := end - start + 1
	if want <= 0 {
		return nil, fmt.Errorf("imagearchive: invalid range %d-%d", start, end)
	}
	if want > f.remaining {
		return nil, fmt.Errorf("imagearchive: range %d-%d exceeds the %d byte budget", start, end, int64(maxFetchBytes))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		// Full body: abandon it without reading, the archive may be gigabytes.
		resp.Body.Close()
		return nil, errRangeIgnored
	}
	defer closeBody(resp)
	if resp.StatusCode != http.StatusPartialContent {
		return nil, &UpstreamStatusError{Method: http.MethodGet, Status: resp.StatusCode}
	}
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		got, ok := contentRangeStart(cr)
		if !ok || got != start {
			return nil, fmt.Errorf("GET %s bytes=%d-%d: unexpected Content-Range %q", f.url, start, end, cr)
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, want))
	f.remaining -= int64(len(data))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// contentRangeStart parses the first offset of a "bytes start-end/total" header.
func contentRangeStart(v string) (int64, bool) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "bytes ") {
		return 0, false
	}
	v = strings.TrimPrefix(v, "bytes ")
	dash := strings.IndexByte(v, '-')
	if dash <= 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(v[:dash], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// closeBody drains a small amount so the connection can be reused, then closes.
func closeBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
}

// IsInvalidURL reports whether err rejects the URL itself — a scheme that is
// not http/https, a missing host, embedded credentials — rather than a network
// or server failure. Callers use it to separate "your input is wrong" from
// "the mirror did not answer".
func IsInvalidURL(err error) bool { return errors.Is(err, ErrInvalidURL) }
