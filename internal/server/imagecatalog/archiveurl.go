package imagecatalog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
	"gpuworks.ai/gpuworks/pkg/imagearchive"
)

// FetchSourceURL is the ImageNode.Source value recorded for an image a node
// downloaded itself from an archive URL; FetchSourceURLRelay is the value for
// the same archive streamed through the management server.
const (
	FetchSourceURL      = "url"
	FetchSourceURLRelay = "url-relay"
)

// archiveRequestTimeout bounds one preview or HEAD against an operator
// supplied archive URL. The requests read at most ~1.2 MiB, so a slow mirror
// is the only thing this waits on.
const archiveRequestTimeout = 30 * time.Second

// ArchivePeek is what can be learned about a remote image archive without
// downloading it.
type ArchivePeek = imagearchive.Peek

// FetchRequest is how one node is asked to acquire an image. Source is empty
// (pull it from wherever the node's pull source rules say) or FetchSourceURL;
// ArchiveURL and Relay apply only to the latter. It travels from the API layer
// to the hub as one value so adding a way to fetch an image never means adding
// another collaborator to the API's dependency set.
type FetchRequest struct {
	Source     string
	ArchiveURL string
	Relay      bool
	// Copies are the references that name the image beyond the catalog's own
	// (a deployment's template sources, ADR 0038); nil means the catalog's.
	Copies []string
	// Force pulls again even when the node already holds the image: a
	// rolling tag (nightly, latest) moves upstream while the local copy stays.
	// A transfer still in progress is never started twice.
	Force bool
}

// IsURLFetch reports whether the request asks for an archive URL rather than a
// registry pull.
func (r FetchRequest) IsURLFetch() bool {
	return strings.EqualFold(strings.TrimSpace(r.Source), FetchSourceURL)
}

// IsArchiveURL reports whether a registry_sources entry addresses an image
// archive over HTTP rather than naming a registry reference. Every consumer of
// registry_sources asks this one question so a URL can never reach a pull.
func IsArchiveURL(entry string) bool { return pullref.IsArchiveURL(entry) }

// NormalizeArchiveURL is the single spelling rule for an archive URL stored in
// registry_sources: surrounding space is removed, a fragment is dropped (it is
// never sent to a server and would make two spellings of one address), and
// anything that is not a credential-free http/https URL is rejected. The query
// string is preserved: object stores routinely address a file with one.
func NormalizeArchiveURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("archive url is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("archive url is not a valid URL: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("archive url scheme %q is not http or https", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("archive url has no host")
	}
	if u.User != nil {
		return "", fmt.Errorf("archive url must not embed credentials")
	}
	if strings.TrimSpace(u.Path) == "" || u.Path == "/" {
		return "", fmt.Errorf("archive url has no path")
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String(), nil
}

// ArchiveURLHost is the operator-facing short name for an archive URL.
func ArchiveURLHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.TrimSpace(raw)
	}
	return u.Host
}

// ArchiveURLHostname is the allow-list name for an archive URL. It has no port.
func ArchiveURLHostname(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// UpstreamStatus reports the archive server's HTTP status when err is one.
// Transport failures and URL rejections return ok false.
func UpstreamStatus(err error) (int, bool) {
	var status *imagearchive.UpstreamStatusError
	if !errors.As(err, &status) || status == nil || status.Status == 0 {
		return 0, false
	}
	return status.Status, true
}

// ArchiveHTTPClient is the client every request against an operator-supplied
// archive URL uses: bounded in time and in redirect hops, with each hop's
// target re-checked against the scheme and credential rules.
func ArchiveHTTPClient() *http.Client {
	return &http.Client{Timeout: archiveRequestTimeout, CheckRedirect: imagearchive.CheckRedirect}
}

// ArchiveStreamHTTPClient is the client for reading an archive's whole body.
// It shares ArchiveHTTPClient's redirect bound and per-hop scheme and
// credential re-check, and deliberately drops the overall timeout: a
// multi-gigabyte archive legitimately takes longer than any header request,
// and the caller's context is what bounds it.
func ArchiveStreamHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: imagearchive.CheckRedirect}
}

// PeekArchiveURL reads a remote archive's size and repository tags without
// downloading it. A peek that cannot determine the tags is not an error: the
// returned ArchivePeek reports Previewable false and the caller falls back to
// asking the operator which reference the archive carries.
func PeekArchiveURL(ctx context.Context, client *http.Client, rawURL string) (ArchivePeek, error) {
	if client == nil {
		client = ArchiveHTTPClient()
	}
	return imagearchive.PeekURL(ctx, client, rawURL)
}

// ArchiveURLSize reports the archive's size with a single HEAD request.
func ArchiveURLSize(ctx context.Context, client *http.Client, rawURL string) (int64, error) {
	if client == nil {
		client = ArchiveHTTPClient()
	}
	return imagearchive.HeadSize(ctx, client, rawURL)
}

// ArchiveURLInvalid reports whether err came from rejecting the URL itself
// rather than from the network, so a caller can answer 400 instead of 502.
func ArchiveURLInvalid(err error) bool { return imagearchive.IsInvalidURL(err) }
