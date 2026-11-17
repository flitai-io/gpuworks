package modelrepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Recursion limits for an HTTP directory index. A plain file server offers no
// way to ask "how big is this tree?" before walking it, so the walk is bounded
// on every axis instead. Exceeding any bound is an explicit error: a truncated
// listing would silently register a model that is missing weights.
const (
	// httpDirMaxDepth is how many directory levels below the listed root are
	// walked. Level 0 is the root itself; a directory discovered below level
	// httpDirMaxDepth aborts the listing.
	httpDirMaxDepth = 3
	// httpDirMaxEntries bounds the files plus directories discovered in total.
	httpDirMaxEntries = 5000
	// httpDirConcurrency bounds simultaneous index requests to one server.
	httpDirConcurrency = 4
	// httpDirMaxBody bounds one index response. Autoindex pages are a few
	// hundred KiB even for repositories with hundreds of shards.
	httpDirMaxBody = 8 << 20
	// httpDirTimeout bounds one index request.
	httpDirTimeout = 30 * time.Second
)

// ErrListingTooLarge reports that a directory index exceeded one of the walk's
// bounds. A caller has to tell this apart from an unreachable host: the tree is
// there and readable, it is simply larger than a model repository ever is, and
// the operator needs to be told that rather than that the URL was wrong.
var ErrListingTooLarge = errors.New("http directory listing exceeds its limits")

// ErrEmptyListing reports a directory index that reads fine but lists no file.
// Like ErrListingTooLarge it is not an unreachable host: the URL answered, it
// just holds nothing to register.
var ErrEmptyListing = errors.New("http directory listing returned no files")

// httpDirEntry is one accepted row of a directory index, already validated to
// live inside the listed root.
type httpDirEntry struct {
	// escapedRel is the entry's path relative to the root, still percent
	// encoded, and is what download URLs are built from.
	escapedRel string
	// rel is the decoded form, which is what the caller sees and what the
	// agent writes to disk.
	rel   string
	isDir bool
	size  int64
}

// SizeIsApproximate reports whether a listing's sizes are rounded display
// values rather than exact byte counts. An HTTP directory index prints sizes
// for humans ("1.4G"), so every consumer that budgets disk or renders progress
// has to treat them as estimates and correct them from the download response.
func SizeIsApproximate(source string) bool { return source == SourceURL }

// NormalizeDirectoryURL is the single spelling of a model directory URL. It is
// the only producer of the stored directory URL, so the server and the agent
// can never disagree about which directory a model came from: the same
// directory written with or without a trailing slash, with a sort query string
// or with a fragment all normalize to one value.
//
// Credentials embedded in the URL are rejected rather than stripped, because
// silently dropping them would turn an authenticated mirror into an anonymous
// one that fails much later, on a node, with an unrelated error.
func NormalizeDirectoryURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("model directory URL is empty")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("model directory URL %q is not a valid URL: %w", raw, err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("model directory URL %q must use http or https", raw)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("model directory URL must not embed credentials")
	}
	if parsed.Hostname() == "" {
		return "", fmt.Errorf("model directory URL %q has no host", raw)
	}
	// Host case and an explicit port are preserved: a mirror may be addressed
	// by a name that only resolves in one spelling, and dropping the port
	// would point at an entirely different service.
	return scheme + "://" + parsed.Host + strings.TrimRight(parsed.EscapedPath(), "/"), nil
}

// SuggestRepoID proposes the repository id a directory URL stands for, using
// the last two path segments ("Qwen/Qwen3-0.6B"). It is only a default: the
// repository id is the identity the inference catalog and the template matcher
// key on, so the operator confirms or corrects it before registration.
func SuggestRepoID(normalizedURL string) string {
	path := strings.TrimSpace(normalizedURL)
	if parsed, err := url.Parse(path); err == nil && parsed.Host != "" {
		path = parsed.Path
	}
	segments := make([]string, 0, 2)
	for _, segment := range strings.Split(path, "/") {
		if segment = strings.TrimSpace(segment); segment != "" {
			segments = append(segments, segment)
		}
	}
	switch len(segments) {
	case 0:
		return ""
	case 1:
		return segments[0]
	default:
		return segments[len(segments)-2] + "/" + segments[len(segments)-1]
	}
}

// resolveHTTPDirURL builds the download URL for one listed file. It is the
// shape ResolveURL uses for the url source: the endpoint is the directory
// itself, so there is no revision and no resolve endpoint to go through.
// resolveHTTPDirURL joins a listed file path onto the directory URL. The path
// is the decoded name the agent writes to disk, so each segment is re-escaped:
// a shard listed as "weights%20v2.bin" comes back as "weights v2.bin" and must
// go out again as "weights%20v2.bin".
func resolveHTTPDirURL(endpoint, path string) string {
	segments := strings.Split(strings.Trim(strings.TrimSpace(path), "/"), "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.TrimRight(strings.TrimSpace(endpoint), "/") + "/" + strings.Join(segments, "/")
}

type httpDirLister struct {
	base    string
	baseURL *url.URL
	headers map[string]string
	options RequestOptions

	entries int
}

// listHTTPDir walks an HTTP directory index and returns every file below it.
// endpoint is the directory URL itself; repo identity plays no part in the
// listing, because a plain file server has no repository concept.
func listHTTPDir(ctx context.Context, endpoint string, headers map[string]string, options ...RequestOptions) ([]File, error) {
	base, err := NormalizeDirectoryURL(endpoint)
	if err != nil {
		return nil, err
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("model directory URL %q is not a valid URL: %w", base, err)
	}
	lister := &httpDirLister{base: base, baseURL: baseURL, headers: headers}
	if len(options) > 0 {
		lister.options = options[0]
	}
	return lister.walk(ctx)
}

func (l *httpDirLister) walk(ctx context.Context) ([]File, error) {
	var files []File
	level := []string{""}
	for depth := 0; depth <= httpDirMaxDepth && len(level) > 0; depth++ {
		next, found, err := l.listLevel(ctx, level)
		if err != nil {
			return nil, err
		}
		files = append(files, found...)
		level = next
	}
	if len(level) > 0 {
		return nil, fmt.Errorf("%w: maximum depth of %d: url=%s source=%s dir=%q",
			ErrListingTooLarge, httpDirMaxDepth, l.base, SourceURL, level[0])
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: url=%s source=%s", ErrEmptyListing, l.base, SourceURL)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// listLevel fetches one whole directory level, at most httpDirConcurrency
// requests at a time, and returns the directories that make up the next level.
func (l *httpDirLister) listLevel(ctx context.Context, dirs []string) ([]string, []File, error) {
	var (
		mu       sync.Mutex
		next     []string
		files    []File
		firstErr error
		wg       sync.WaitGroup
	)
	slots := make(chan struct{}, httpDirConcurrency)
	for _, dir := range dirs {
		wg.Add(1)
		go func(dir string) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			mu.Lock()
			aborted := firstErr != nil
			mu.Unlock()
			if aborted {
				return
			}
			entries, err := l.listDir(ctx, dir)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			l.entries += len(entries)
			if l.entries > httpDirMaxEntries {
				if firstErr == nil {
					firstErr = fmt.Errorf("%w: maximum of %d entries: url=%s source=%s",
						ErrListingTooLarge, httpDirMaxEntries, l.base, SourceURL)
				}
				return
			}
			for _, entry := range entries {
				if entry.isDir {
					next = append(next, entry.escapedRel)
					continue
				}
				files = append(files, File{Path: entry.rel, Size: entry.size})
			}
		}(dir)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, nil, firstErr
	}
	sort.Strings(next)
	return next, files, nil
}

func (l *httpDirLister) listDir(ctx context.Context, escapedRel string) ([]httpDirEntry, error) {
	dirURL := l.base
	if escapedRel != "" {
		dirURL += "/" + escapedRel
	}
	body, err := l.get(ctx, dirURL)
	if err != nil {
		return nil, err
	}
	return parseDirectoryIndex(body, l.baseURL, dirURL, escapedRel)
}

func (l *httpDirLister) get(ctx context.Context, rawURL string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, httpDirTimeout)
	defer cancel()
	// A file server serves a directory index only for the trailing-slash form;
	// without it Apache and nginx answer 301 to the slash form, which costs an
	// extra round trip per directory.
	requestURL := rawURL + "/"
	if l.options.Rewrite != nil {
		requestURL = l.options.Rewrite(requestURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	for key, value := range l.headers {
		req.Header.Set(key, value)
	}
	client := http.DefaultClient
	if l.options.Client != nil {
		client = l.options.Client
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("http directory listing failed: url=%s source=%s: %w", requestURL, SourceURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", throttleError(resp, requestURL, fmt.Sprintf("http directory listing: http %d: url=%s source=%s", resp.StatusCode, requestURL, SourceURL))
	}
	if contentType := resp.Header.Get("Content-Type"); contentType != "" && !strings.Contains(strings.ToLower(contentType), "html") {
		return "", fmt.Errorf("http directory listing is not an HTML directory index: content-type=%s url=%s source=%s",
			contentType, requestURL, SourceURL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, httpDirMaxBody+1))
	if err != nil {
		return "", fmt.Errorf("http directory listing failed: url=%s source=%s: %w", requestURL, SourceURL, err)
	}
	if len(body) > httpDirMaxBody {
		return "", fmt.Errorf("%w: one response exceeds %d bytes: url=%s source=%s",
			ErrListingTooLarge, httpDirMaxBody, requestURL, SourceURL)
	}
	return string(body), nil
}
